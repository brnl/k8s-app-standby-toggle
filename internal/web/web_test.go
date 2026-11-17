package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brnl/k8s-app-standby-toggle/internal/auth"
	"github.com/brnl/k8s-app-standby-toggle/internal/config"
	"github.com/brnl/k8s-app-standby-toggle/internal/controller"
	"github.com/brnl/k8s-app-standby-toggle/internal/kube"
	"github.com/brnl/k8s-app-standby-toggle/internal/kube/kubetest"
	"github.com/brnl/k8s-app-standby-toggle/internal/standby"
	"github.com/brnl/k8s-app-standby-toggle/internal/state"
)

const (
	ciSubject = "system:serviceaccount:ci-runner:ci-runner"
	prefix    = "k8s-app-standby-toggle.cloud64.net/"
)

type env struct {
	t       *testing.T
	api     *kubetest.Server
	ctrl    *controller.Controller
	authn   *auth.Authenticator
	handler http.Handler
	session *http.Cookie
	csrf    string
}

func newEnv(t *testing.T, disabled bool) *env {
	t.Helper()
	api := kubetest.New(t)
	api.TokenReview = func(token string, auds []string) (bool, string, []string) {
		switch token {
		case "ci":
			return true, ciSubject, auds
		case "stranger":
			return true, "system:serviceaccount:default:default", auds
		}
		return false, "", nil
	}
	client := api.Client()
	api.Set(kube.Path("apps", "v1", "deployments", "vik", "web"), map[string]any{
		"metadata": map[string]any{"name": "web", "namespace": "vik", "generation": 1},
		"spec":     map[string]any{"replicas": 1},
		"status":   map[string]any{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1},
	})
	api.Set(kube.Path("argoproj.io", "v1alpha1", "applications", "argocd", "vikunja"), map[string]any{
		"metadata": map[string]any{"name": "vikunja"},
		"status": map[string]any{"resources": []any{
			map[string]any{"group": "apps", "version": "v1", "kind": "Deployment", "namespace": "vik", "name": "web"},
		}},
	})
	ctrl := controller.New(controller.Options{
		Groups: []config.Group{
			{Name: "identity", Default: config.AlwaysOn, Apps: []string{"authelia"}},
			{Name: "vikunja", Default: config.Asleep, Apps: []string{"vikunja", "vikunja-infra"}},
		},
		ArgoCDNamespace: "argocd", Namespace: "standby",
		Client: client, Store: state.NewMemoryStore(), Actuator: standby.New(client, prefix),
		Interval: 30 * time.Second, DegradedAfter: 10 * time.Minute, DatabaseWait: 5 * time.Minute,
		ConflictThreshold: 3, ConflictWindow: 10 * time.Minute,
	})
	authn := &auth.Authenticator{
		Disabled:   disabled,
		Signer:     auth.NewSigner(nil, nil),
		Reviewer:   auth.NewTokenReviewer(client, "k8s-app-standby-toggle", []string{ciSubject}, nil),
		SessionTTL: time.Hour,
		Log:        slog.New(slog.DiscardHandler),
	}
	e := &env{t: t, api: api, ctrl: ctrl, authn: authn}
	e.handler = Handler(Options{
		Controller: ctrl, Auth: authn,
		WakeDurations: config.DefaultWakeDurations, HoldDefault: 24 * time.Hour, HoldMax: 24 * time.Hour, LeaseMax: 4 * time.Hour,
		Version: "test",
	})
	sess := authn.Signer.NewSession("bram", time.Hour)
	e.session = &http.Cookie{Name: auth.SessionCookie, Value: authn.Signer.EncodeSession(sess)}
	e.csrf = sess.CSRF
	return e
}

type caller int

const (
	nobody caller = iota
	person
	personNoCSRF
	machine
	stranger
)

func (e *env) do(who caller, method, path, body string) (int, map[string]any) {
	e.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	switch who {
	case person:
		req.AddCookie(e.session)
		req.Header.Set(auth.CSRFHeader, e.csrf)
	case personNoCSRF:
		req.AddCookie(e.session)
	case machine:
		req.Header.Set("Authorization", "Bearer ci")
	case stranger:
		req.Header.Set("Authorization", "Bearer stranger")
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestAuthenticationErrors(t *testing.T) {
	e := newEnv(t, false)
	for _, tt := range []struct {
		who  caller
		want int
	}{{nobody, 401}, {stranger, 403}, {person, 200}, {machine, 200}} {
		if code, _ := e.do(tt.who, "GET", "/api/v1/groups", ""); code != tt.want {
			t.Errorf("caller %d: %d, want %d", tt.who, code, tt.want)
		}
	}
	req := httptest.NewRequest("GET", "/api/v1/groups", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("invalid token: %d", rec.Code)
	}
}

func TestPermissions(t *testing.T) {
	e := newEnv(t, false)
	tests := []struct {
		name   string
		who    caller
		method string
		path   string
		body   string
		want   int
	}{
		{"machine creates lease", machine, "PUT", "/api/v1/groups/vikunja/leases/ci-run-123", `{"ttl":"45m"}`, 200},
		{"machine reads group", machine, "GET", "/api/v1/groups/vikunja", "", 200},
		{"machine sets hold", machine, "PUT", "/api/v1/groups/vikunja/hold", `{"reason":"run 123 failed","ttl":"1h"}`, 200},
		{"machine deletes lease", machine, "DELETE", "/api/v1/groups/vikunja/leases/ci-run-123", "", 200},
		{"machine may not set override", machine, "PUT", "/api/v1/groups/vikunja/override", `{"state":"awake","ttl":"1h"}`, 403},
		{"machine may not delete override", machine, "DELETE", "/api/v1/groups/vikunja/override", "", 403},
		{"machine may not lift hold", machine, "DELETE", "/api/v1/groups/vikunja/hold", "", 403},
		{"machine may not clear conflict", machine, "DELETE", "/api/v1/groups/vikunja/conflict", "", 403},
		{"person needs CSRF", personNoCSRF, "PUT", "/api/v1/groups/vikunja/override", `{"state":"awake","ttl":"1h"}`, 403},
		{"person sets override", person, "PUT", "/api/v1/groups/vikunja/override", `{"state":"awake","ttl":"1h"}`, 200},
		{"person deletes override", person, "DELETE", "/api/v1/groups/vikunja/override", "", 200},
		{"person lifts hold", person, "DELETE", "/api/v1/groups/vikunja/hold", "", 200},
		{"person clears conflict", person, "DELETE", "/api/v1/groups/vikunja/conflict", "", 200},
		{"always-on override", person, "PUT", "/api/v1/groups/identity/override", `{"state":"asleep"}`, 409},
		{"always-on lease", machine, "PUT", "/api/v1/groups/identity/leases/x", `{"ttl":"1h"}`, 409},
		{"always-on hold", person, "PUT", "/api/v1/groups/identity/hold", `{"reason":"x"}`, 409},
		{"always-on back to default", person, "DELETE", "/api/v1/groups/identity/override", "", 409},
		{"unknown group", person, "GET", "/api/v1/groups/nope", "", 404},
		{"unknown group lease", machine, "PUT", "/api/v1/groups/nope/leases/x", `{"ttl":"1h"}`, 404},
		{"unknown endpoint", person, "GET", "/api/v1/nothing", "", 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, out := e.do(tt.who, tt.method, tt.path, tt.body); code != tt.want {
				t.Fatalf("%d %v, want %d", code, out, tt.want)
			}
		})
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t, false)
	tests := []struct {
		name, method, path, body, want string
	}{
		{"wake without ttl", "PUT", "/api/v1/groups/vikunja/override", `{"state":"awake"}`, "ttl is required"},
		{"wake for a disallowed duration", "PUT", "/api/v1/groups/vikunja/override", `{"state":"awake","ttl":"2h"}`, "ttl must be one of 1h, 4h, 24h"},
		{"unknown state", "PUT", "/api/v1/groups/vikunja/override", `{"state":"on"}`, `state must be "awake" or "asleep"`},
		{"bad duration", "PUT", "/api/v1/groups/vikunja/leases/x", `{"ttl":"soon"}`, "invalid ttl"},
		{"negative duration", "PUT", "/api/v1/groups/vikunja/leases/x", `{"ttl":"-5m"}`, "positive"},
		{"lease too long", "PUT", "/api/v1/groups/vikunja/leases/x", `{"ttl":"5h"}`, "must not exceed 4h0m0s"},
		{"lease without ttl", "PUT", "/api/v1/groups/vikunja/leases/x", `{}`, "ttl is required"},
		{"bad owner", "PUT", "/api/v1/groups/vikunja/leases/-x-", `{"ttl":"5m"}`, "invalid lease owner"},
		{"hold without reason", "PUT", "/api/v1/groups/vikunja/hold", `{"ttl":"1h"}`, "reason is required"},
		{"hold too long", "PUT", "/api/v1/groups/vikunja/hold", `{"reason":"x","ttl":"25h"}`, "must not exceed 24h"},
		{"multi-line reason", "PUT", "/api/v1/groups/vikunja/hold", `{"reason":"a\nb"}`, "single line"},
		{"unknown field", "PUT", "/api/v1/groups/vikunja/hold", `{"reason":"x","until":"never"}`, "unknown field"},
		{"no body", "PUT", "/api/v1/groups/vikunja/leases/x", ``, "must be a JSON object"},
		{"trailing data", "PUT", "/api/v1/groups/vikunja/leases/x", `{"ttl":"5m"}{}`, "unexpected data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out := e.do(person, tt.method, tt.path, tt.body)
			if code != 400 || !strings.Contains(out["error"].(string), tt.want) {
				t.Fatalf("%d %v, want 400 containing %q", code, out, tt.want)
			}
		})
	}
}

func TestPipelineFlow(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	e.ctrl.Reconcile(ctx)
	deploy := kube.Path("apps", "v1", "deployments", "vik", "web")
	settle := func() {
		o := e.api.Object(deploy)
		r := o["spec"].(map[string]any)["replicas"]
		o["status"] = map[string]any{"observedGeneration": 1, "replicas": r, "readyReplicas": r}
		e.api.Set(deploy, o)
	}
	settle()
	e.ctrl.Reconcile(ctx)

	// 1. Take a lease.
	code, g := e.do(machine, "PUT", "/api/v1/groups/vikunja/leases/ci-run-123", `{"ttl":"45m"}`)
	if code != 200 || g["desired"] != "awake" || g["phase"] != "waking" || g["ready"] != false {
		t.Fatalf("lease: %d %v", code, g)
	}
	leases := g["leases"].([]any)
	if len(leases) != 1 || leases[0].(map[string]any)["owner"] != "ci-run-123" || leases[0].(map[string]any)["by"] != ciSubject {
		t.Fatalf("leases = %v", leases)
	}
	if !strings.HasPrefix(g["reason"].(string), "lease ci-run-123 until ") || g["override"] != nil || g["hold"] != nil {
		t.Fatalf("group = %v", g)
	}

	// 2. Poll until awake and ready.
	e.ctrl.Reconcile(ctx)
	settle()
	e.ctrl.Reconcile(ctx)
	_, g = e.do(machine, "GET", "/api/v1/groups/vikunja", "")
	if g["phase"] != "awake" || g["ready"] != true {
		t.Fatalf("not ready: %v", g)
	}
	res := g["resources"].([]any)[0].(map[string]any)
	if res["kind"] != "Deployment" || res["namespace"] != "vik" || res["name"] != "web" || res["replicas"] != 1.0 || res["ready"] != 1.0 {
		t.Fatalf("resource = %v", res)
	}
	if m := g["missing"].([]any); len(m) != 1 || m[0] != "vikunja-infra" {
		t.Fatalf("missing = %v", m)
	}

	// 4. Failed: hold with the run URL, then release the lease.
	if code, _ = e.do(machine, "PUT", "/api/v1/groups/vikunja/hold", `{"reason":"https://ci.example.com/run/123","ttl":"24h"}`); code != 200 {
		t.Fatalf("hold: %d", code)
	}
	code, g = e.do(machine, "DELETE", "/api/v1/groups/vikunja/leases/ci-run-123", "")
	if code != 200 || g["desired"] != "awake" || g["hold"].(map[string]any)["by"] != ciSubject {
		t.Fatalf("after release: %d %v", code, g)
	}

	// A machine cannot shorten the hold either.
	_, g = e.do(machine, "PUT", "/api/v1/groups/vikunja/hold", `{"reason":"retry","ttl":"1h"}`)
	exp, _ := time.Parse(time.RFC3339, g["hold"].(map[string]any)["expires"].(string))
	if time.Until(exp) < 23*time.Hour {
		t.Fatalf("machine shortened the hold to %s", exp)
	}
	// A person lifts it; the group goes back to sleep.
	if code, g = e.do(person, "DELETE", "/api/v1/groups/vikunja/hold", ""); code != 200 || g["desired"] != "asleep" {
		t.Fatalf("lift: %d %v", code, g)
	}
}

func TestPage(t *testing.T) {
	e := newEnv(t, false)
	e.ctrl.Reconcile(context.Background())

	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/auth/login") {
		t.Fatalf("anonymous page: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	e.do(person, "PUT", "/api/v1/groups/vikunja/hold", `{"reason":"test failed, run 123"}`)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(e.session)
	rec = httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("page: %d", rec.Code)
	}
	for _, want := range []string{
		`<meta name="csrf-token" content="` + e.csrf + `">`,
		`data-group="vikunja"`, `held: test failed, run 123`, `by bram`,
		`data-action="wake" data-ttl="1h"`, `data-action="wake" data-ttl="24h"`, `Lift hold`,
		`vikunja-infra (missing)`, `name="csrf_token"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The always-on row has no controls.
	identity := body[strings.Index(body, `data-group="identity"`):strings.Index(body, `data-group="vikunja"`)]
	if strings.Contains(identity, "data-action") || !strings.Contains(identity, "always on") {
		t.Errorf("always-on row: %s", identity)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}

	req = httptest.NewRequest("GET", "/ui/groups", nil)
	req.AddCookie(e.session)
	rec = httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `data-group="vikunja"`) || strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("fragment: %d", rec.Code)
	}

	for _, path := range []string{"/static/app.js", "/static/style.css"} {
		rec = httptest.NewRecorder()
		e.handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}

func TestMetricsAndHealth(t *testing.T) {
	e := newEnv(t, false)
	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String()
	}
	if code, _ := get("/readyz"); code != 503 {
		t.Fatalf("ready before the first reconcile: %d", code)
	}
	if _, body := get("/metrics"); strings.Contains(body, `group_should_be_awake{`) {
		t.Fatal("group metrics before the state was loaded")
	}
	e.ctrl.Reconcile(context.Background())
	e.do(machine, "PUT", "/api/v1/groups/vikunja/hold", `{"reason":"x"}`)
	if code, _ := get("/healthz"); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	if code, _ := get("/readyz"); code != 200 {
		t.Fatalf("readyz: %d", code)
	}
	code, body := get("/metrics")
	if code != 200 {
		t.Fatalf("metrics: %d", code)
	}
	for _, want := range []string{
		`k8s_app_standby_toggle_group_should_be_awake{group="identity"} 1`,
		`k8s_app_standby_toggle_group_should_be_awake{group="vikunja"} 1`,
		`k8s_app_standby_toggle_group_held{group="vikunja"} 1`,
		`k8s_app_standby_toggle_group_conflict{group="vikunja"} 0`,
		`k8s_app_standby_toggle_group_ready{group="vikunja"} 0`,
		`k8s_app_standby_toggle_group_phase{group="vikunja",phase="waking"} 1`,
		`k8s_app_standby_toggle_application_should_be_awake{application="vikunja",group="vikunja"} 1`,
		`k8s_app_standby_toggle_application_missing{application="vikunja-infra",group="vikunja"} 1`,
		`k8s_app_standby_toggle_reconcile_errors_total 0`,
		`k8s_app_standby_toggle_build_info{goversion="`,
		`# TYPE k8s_app_standby_toggle_reconcile_errors_total counter`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestAuthDisabled(t *testing.T) {
	e := newEnv(t, true)
	if code, _ := e.do(nobody, "PUT", "/api/v1/groups/vikunja/override", `{"state":"awake","ttl":"1h"}`); code != 200 {
		t.Fatalf("PUT: %d", code)
	}
	// Still protected against cross-site requests from a browser.
	req := httptest.NewRequest("PUT", "/api/v1/groups/vikunja/override", strings.NewReader(`{"state":"asleep"}`))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("cross-site PUT: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	e.handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("page: %d", rec.Code)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{time.Hour: "1h", 90 * time.Minute: "90m", 24 * time.Hour: "24h", 90 * time.Second: "1m30s"} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %s", d, got)
		}
	}
}
