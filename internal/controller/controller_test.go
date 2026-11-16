package controller

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brnl/k8s-app-standby-toggle/internal/config"
	"github.com/brnl/k8s-app-standby-toggle/internal/kube"
	"github.com/brnl/k8s-app-standby-toggle/internal/kube/kubetest"
	"github.com/brnl/k8s-app-standby-toggle/internal/standby"
	"github.com/brnl/k8s-app-standby-toggle/internal/state"
)

const prefix = "k8s-app-standby-toggle.cloud64.net/"

var ctx = context.Background()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t     *testing.T
	api   *kubetest.Server
	clock *clock
	c     *Controller
}

type res struct{ group, kind, ns, name string }

func deploy(ns, name string) res { return res{"apps", "Deployment", ns, name} }
func sts(ns, name string) res    { return res{"apps", "StatefulSet", ns, name} }
func db(ns, name string) res     { return res{"postgresql.cnpg.io", "Cluster", ns, name} }
func cron(ns, name string) res   { return res{"batch", "CronJob", ns, name} }

func (r res) path() string {
	k, _ := standby.KindOf(r.group, r.kind)
	switch k {
	case standby.Deployment:
		return kube.Path("apps", "v1", "deployments", r.ns, r.name)
	case standby.StatefulSet:
		return kube.Path("apps", "v1", "statefulsets", r.ns, r.name)
	case standby.CronJob:
		return kube.Path("batch", "v1", "cronjobs", r.ns, r.name)
	case standby.Database:
		return kube.Path("postgresql.cnpg.io", "v1", "clusters", r.ns, r.name)
	}
	return ""
}

func newHarness(t *testing.T, groups []config.Group) *harness {
	t.Helper()
	h := &harness{t: t, api: kubetest.New(t), clock: &clock{t: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}}
	client := h.api.Client()
	h.c = New(Options{
		Groups:            groups,
		ArgoCDNamespace:   "argocd",
		Namespace:         "standby",
		Client:            client,
		Store:             state.NewMemoryStore(),
		Actuator:          standby.New(client, prefix),
		Interval:          30 * time.Second,
		DegradedAfter:     10 * time.Minute,
		DatabaseWait:      5 * time.Minute,
		ConflictThreshold: 3,
		ConflictWindow:    10 * time.Minute,
		Now:               h.clock.Now,
		Log:               slog.New(slog.DiscardHandler),
	})
	return h
}

// app creates an Argo CD Application with the given resources, and the
// resources themselves (running).
func (h *harness) app(name string, resources ...res) {
	var list []any
	for _, r := range resources {
		list = append(list, map[string]any{"group": r.group, "version": "v1", "kind": r.kind, "namespace": r.ns, "name": r.name})
		if r.path() != "" && h.api.Object(r.path()) == nil {
			h.create(r, 2)
		}
	}
	h.api.Set(kube.Path("argoproj.io", "v1alpha1", "applications", "argocd", name), map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "argocd"},
		"status":   map[string]any{"resources": list},
	})
}

func (h *harness) create(r res, replicas int) {
	meta := map[string]any{"name": r.name, "namespace": r.ns, "generation": 1}
	switch r.kind {
	case "Cluster":
		h.api.Set(r.path(), map[string]any{"metadata": meta, "spec": map[string]any{"instances": replicas},
			"status": map[string]any{"phase": "Cluster in healthy state", "instances": replicas, "readyInstances": replicas}})
	case "CronJob":
		h.api.Set(r.path(), map[string]any{"metadata": meta, "spec": map[string]any{"schedule": "@daily", "suspend": false}})
	default:
		h.api.Set(r.path(), map[string]any{"metadata": meta, "spec": map[string]any{"replicas": replicas},
			"status": map[string]any{"observedGeneration": 1, "replicas": replicas, "readyReplicas": replicas}})
	}
}

// settle plays the Kubernetes controllers and the CNPG operator: every
// object's status catches up with its spec.
func (h *harness) settle(rs ...res) {
	for _, r := range rs {
		o := h.api.Object(r.path())
		spec, _ := o["spec"].(map[string]any)
		switch r.kind {
		case "Cluster":
			ann, _ := o["metadata"].(map[string]any)["annotations"].(map[string]any)
			if ann["cnpg.io/hibernation"] == "on" {
				o["status"] = map[string]any{"phase": "Cluster in healthy state", "readyInstances": 0,
					"conditions": []any{map[string]any{"type": "cnpg.io/hibernation", "status": "True", "reason": "Hibernated"}}}
			} else {
				o["status"] = map[string]any{"phase": "Cluster in healthy state", "readyInstances": spec["instances"]}
			}
		case "CronJob":
			continue
		default:
			o["status"] = map[string]any{"observedGeneration": o["metadata"].(map[string]any)["generation"],
				"replicas": spec["replicas"], "readyReplicas": spec["replicas"]}
		}
		h.api.Set(r.path(), o)
	}
}

func (h *harness) replicas(r res) float64 {
	h.t.Helper()
	v, _ := h.api.Object(r.path())["spec"].(map[string]any)["replicas"].(float64)
	return v
}

func (h *harness) hibernation(r res) any {
	ann, _ := h.api.Object(r.path())["metadata"].(map[string]any)["annotations"].(map[string]any)
	return ann["cnpg.io/hibernation"]
}

func (h *harness) reconcile() {
	h.t.Helper()
	h.c.Reconcile(ctx)
}

func (h *harness) view(group string) GroupView {
	h.t.Helper()
	v, ok := h.c.View(group)
	if !ok {
		h.t.Fatalf("group %s not found", group)
	}
	return v
}

func (h *harness) expectPhase(group, phase string, ready bool) {
	h.t.Helper()
	v := h.view(group)
	if v.Phase != phase || v.Ready != ready {
		h.t.Fatalf("%s: phase=%s ready=%v (%s), want phase=%s ready=%v; errors %v", group, v.Phase, v.Ready, v.Reason, phase, ready, v.Errors)
	}
}

func (h *harness) mutate(group string, fn func(g *state.Group, now time.Time)) {
	h.t.Helper()
	if _, err := h.c.Mutate(ctx, group, func(g *state.Group, now time.Time) error { fn(g, now); return nil }); err != nil {
		h.t.Fatal(err)
	}
}

func TestAsleepByDefaultAfterStartup(t *testing.T) {
	web, worker := deploy("next", "web"), sts("next", "worker")
	h := newHarness(t, []config.Group{{Name: "nextcloud", Default: config.Asleep, Apps: []string{"nextcloud"}}})
	h.app("nextcloud", web, worker)

	h.expectPhase("nextcloud", PhaseUnknown, false)
	h.reconcile()
	if h.replicas(web) != 0 || h.replicas(worker) != 0 {
		t.Fatal("not scaled to 0 on startup")
	}
	h.expectPhase("nextcloud", PhaseSleeping, false)
	h.settle(web, worker)
	h.reconcile()
	h.expectPhase("nextcloud", PhaseAsleep, false)
}

func TestWakeForAnHourThenAutoSnooze(t *testing.T) {
	web := deploy("next", "web")
	h := newHarness(t, []config.Group{{Name: "nextcloud", Default: config.Asleep, Apps: []string{"nextcloud"}}})
	h.app("nextcloud", web)
	h.reconcile()
	h.settle(web)
	h.reconcile()

	h.mutate("nextcloud", func(g *state.Group, now time.Time) {
		exp := now.Add(time.Hour)
		g.Override = &state.Override{State: state.StateAwake, Expires: &exp, By: "bram", Set: now}
	})
	v := h.view("nextcloud")
	if v.Desired != "awake" || v.Phase != PhaseWaking || v.Reason != "awake until 2026-11-16T13:00:00Z, woken by bram" {
		t.Fatalf("after override: %+v", v)
	}
	h.reconcile()
	if h.replicas(web) != 2 {
		t.Fatalf("replicas = %v, want the recorded 2", h.replicas(web))
	}
	h.expectPhase("nextcloud", PhaseWaking, false)
	h.settle(web)
	h.reconcile()
	h.expectPhase("nextcloud", PhaseAwake, true)

	h.clock.Add(59 * time.Minute)
	h.reconcile()
	h.expectPhase("nextcloud", PhaseAwake, true)
	h.clock.Add(time.Minute)
	h.reconcile()
	if h.replicas(web) != 0 {
		t.Fatal("not asleep after the hour")
	}
	if v := h.view("nextcloud"); v.Override != nil || v.Desired != "asleep" {
		t.Fatalf("override kept: %+v", v)
	}
}

func TestLeaseAndHold(t *testing.T) {
	web := deploy("vik", "web")
	h := newHarness(t, []config.Group{{Name: "vikunja", Default: config.Asleep, Apps: []string{"vikunja", "vikunja-infra"}}})
	h.app("vikunja", web)
	h.app("vikunja-infra")
	h.reconcile()

	lease := func(now time.Time, ttl time.Duration) {
		h.mutate("vikunja", func(g *state.Group, now time.Time) {
			g.Leases = map[string]state.Lease{"ci-run-123": {By: "system:serviceaccount:ci:ci", Expires: now.Add(ttl), Created: now}}
		})
	}

	// A lease wakes the group; deleting it puts it back to sleep.
	lease(h.clock.Now(), 45*time.Minute)
	h.reconcile()
	if h.replicas(web) != 2 {
		t.Fatal("lease did not wake the group")
	}
	if v := h.view("vikunja"); len(v.Leases) != 1 || v.Leases[0].Owner != "ci-run-123" || !strings.HasPrefix(v.Reason, "lease ci-run-123 until") {
		t.Fatalf("view: %+v", v)
	}
	h.mutate("vikunja", func(g *state.Group, _ time.Time) { delete(g.Leases, "ci-run-123") })
	h.reconcile()
	if h.replicas(web) != 0 {
		t.Fatal("deleting the lease did not put the group to sleep")
	}

	// A lease that is never deleted expires.
	lease(h.clock.Now(), 45*time.Minute)
	h.reconcile()
	h.clock.Add(46 * time.Minute)
	h.reconcile()
	if h.replicas(web) != 0 {
		t.Fatal("expired lease kept the group awake")
	}

	// A hold keeps the group awake after the lease is gone.
	lease(h.clock.Now(), 45*time.Minute)
	h.mutate("vikunja", func(g *state.Group, now time.Time) {
		g.Hold = &state.Hold{Reason: "test failed, run 123", By: "ci", Expires: now.Add(24 * time.Hour), Set: now}
		delete(g.Leases, "ci-run-123")
	})
	h.reconcile()
	h.settle(web)
	h.reconcile()
	h.expectPhase("vikunja", PhaseAwake, true)
	if v := h.view("vikunja"); v.Hold == nil || !strings.Contains(v.Reason, "test failed, run 123") {
		t.Fatalf("hold not shown: %+v", v)
	}
}

func TestDatabaseFirstOnWakeLastOnSleep(t *testing.T) {
	web, pg := deploy("next", "web"), db("next", "pg")
	h := newHarness(t, []config.Group{{Name: "nextcloud", Default: config.Awake, Apps: []string{"nextcloud", "nextcloud-infra"}}})
	h.app("nextcloud", web)
	h.app("nextcloud-infra", pg)
	h.reconcile()
	h.expectPhase("nextcloud", PhaseAwake, true)

	// Sleep: the app first; the database only when the app's pods are gone.
	h.mutate("nextcloud", func(g *state.Group, now time.Time) {
		g.Override = &state.Override{State: state.StateAsleep, By: "bram", Set: now}
	})
	h.reconcile()
	if h.replicas(web) != 0 || h.hibernation(pg) == "on" {
		t.Fatalf("replicas=%v hibernation=%v, want the app asleep and the database still up", h.replicas(web), h.hibernation(pg))
	}
	h.settle(web)
	h.reconcile()
	if h.hibernation(pg) != "on" {
		t.Fatal("database not hibernated after the app")
	}
	h.expectPhase("nextcloud", PhaseSleeping, false)
	h.settle(pg)
	h.reconcile()
	h.expectPhase("nextcloud", PhaseAsleep, false)

	// Wake: the database first; the app once it is ready.
	h.mutate("nextcloud", func(g *state.Group, _ time.Time) { g.Override = nil })
	h.reconcile()
	if h.hibernation(pg) != "off" || h.replicas(web) != 0 {
		t.Fatalf("hibernation=%v replicas=%v, want the database woken first", h.hibernation(pg), h.replicas(web))
	}
	h.reconcile()
	if h.replicas(web) != 0 {
		t.Fatal("app woken before the database was ready")
	}
	h.settle(pg)
	h.reconcile()
	if h.replicas(web) != 2 {
		t.Fatal("app not woken after the database was ready")
	}
	h.settle(web)
	h.reconcile()
	h.expectPhase("nextcloud", PhaseAwake, true)
}

func TestDatabaseWaitIsBounded(t *testing.T) {
	web, pg := deploy("next", "web"), db("next", "pg")
	h := newHarness(t, []config.Group{{Name: "nextcloud", Default: config.Asleep, Apps: []string{"nextcloud"}}})
	h.app("nextcloud", web, pg)
	h.reconcile()
	h.settle(web)
	h.reconcile()
	h.settle(pg)
	h.reconcile()
	h.expectPhase("nextcloud", PhaseAsleep, false)

	h.mutate("nextcloud", func(g *state.Group, now time.Time) {
		exp := now.Add(4 * time.Hour)
		g.Override = &state.Override{State: state.StateAwake, Expires: &exp, By: "bram", Set: now}
	})
	h.reconcile()
	// The database never becomes ready.
	h.clock.Add(4 * time.Minute)
	h.reconcile()
	if h.replicas(web) != 0 {
		t.Fatal("app woken before the wait was over")
	}
	h.clock.Add(time.Minute)
	h.reconcile()
	if h.replicas(web) != 2 {
		t.Fatal("app still waiting after the bound")
	}
	h.clock.Add(5 * time.Minute)
	h.reconcile()
	h.expectPhase("nextcloud", PhaseDegraded, false)
}

func TestNewResourceInSleepingApplication(t *testing.T) {
	web := deploy("next", "web")
	h := newHarness(t, []config.Group{{Name: "nextcloud", Default: config.Asleep, Apps: []string{"nextcloud"}}})
	h.app("nextcloud", web)
	h.reconcile()
	h.settle(web)
	h.reconcile()

	// A chart upgrade adds a Deployment and a CronJob.
	extra, job := deploy("next", "imaginary"), cron("next", "cleanup")
	h.app("nextcloud", web, extra, job)
	h.reconcile()
	if h.replicas(extra) != 0 {
		t.Fatal("new Deployment not put to sleep within one reconcile")
	}
	if s := h.api.Object(job.path())["spec"].(map[string]any)["suspend"]; s != true {
		t.Fatal("new CronJob not suspended")
	}
}

func TestConflictStopsActing(t *testing.T) {
	web := deploy("vik", "web")
	h := newHarness(t, []config.Group{{Name: "vikunja", Default: config.Asleep, Apps: []string{"vikunja"}}})
	h.app("vikunja", web)
	h.reconcile()

	// Argo CD without the prerequisite: self-heal puts the replicas back.
	undo := func() {
		o := h.api.Object(web.path())
		o["spec"].(map[string]any)["replicas"] = 2
		h.api.Set(web.path(), o)
	}
	for i := range 3 {
		undo()
		h.clock.Add(30 * time.Second)
		h.reconcile()
		if i < 2 && h.view("vikunja").Phase == PhaseConflict {
			t.Fatalf("conflict after %d undo(s)", i+1)
		}
	}
	v := h.view("vikunja")
	if v.Phase != PhaseConflict || v.Conflict == nil || !strings.Contains(v.Conflict.Reason, "ignoreDifferences") {
		t.Fatalf("no conflict: %+v", v)
	}

	// It stops acting: no more patches, however often it is undone.
	undo()
	h.api.ResetRequests()
	for range 5 {
		h.clock.Add(30 * time.Second)
		h.reconcile()
	}
	if n := h.api.Count("PATCH", "/"); n != 0 {
		t.Fatalf("%d patches while in conflict", n)
	}
	if h.replicas(web) != 2 {
		t.Fatal("flapping")
	}
	h.expectPhase("vikunja", PhaseConflict, false)

	// Clearing the conflict lets it act again.
	h.mutate("vikunja", func(g *state.Group, _ time.Time) { g.Conflict = nil })
	h.reconcile()
	if h.replicas(web) != 0 {
		t.Fatal("not acting after the conflict was cleared")
	}
	if h.view("vikunja").Phase == PhaseConflict {
		t.Fatal("still in conflict")
	}
}

func TestUndosOutsideTheWindowAreNoConflict(t *testing.T) {
	web := deploy("vik", "web")
	h := newHarness(t, []config.Group{{Name: "vikunja", Default: config.Asleep, Apps: []string{"vikunja"}}})
	h.app("vikunja", web)
	h.reconcile()
	for range 5 {
		o := h.api.Object(web.path())
		o["spec"].(map[string]any)["replicas"] = 1
		h.api.Set(web.path(), o)
		h.clock.Add(6 * time.Minute)
		h.reconcile()
	}
	if h.view("vikunja").Phase == PhaseConflict {
		t.Fatal("occasional undos marked as conflict")
	}
}

func TestSkipRulesAndMissingApplications(t *testing.T) {
	h := newHarness(t, []config.Group{{Name: "all", Default: config.Asleep, Apps: []string{"mixed", "ghost"}}})
	protected := []res{deploy("kube-system", "coredns"), deploy("argocd", "argocd-server"), deploy("standby", "self")}
	ds := res{"apps", "DaemonSet", "app", "agent"}
	svc := res{"", "Service", "app", "web"}
	h.app("mixed", append(protected, ds, svc)...)
	h.reconcile()
	for _, r := range protected {
		if h.replicas(r) != 2 {
			t.Fatalf("%s/%s in a protected namespace was touched", r.ns, r.name)
		}
	}
	if n := h.api.Count("PATCH", "/"); n != 0 {
		t.Fatalf("%d patches", n)
	}
	v := h.view("all")
	if len(v.Missing) != 1 || v.Missing[0] != "ghost" {
		t.Fatalf("missing = %v", v.Missing)
	}
}

func TestAlwaysOnGroupIsWoken(t *testing.T) {
	// A group that is always-on now but was put to sleep earlier is woken.
	web := deploy("id", "authelia")
	h := newHarness(t, []config.Group{{Name: "identity", Default: config.AlwaysOn, Apps: []string{"authelia"}}})
	h.app("authelia", web)
	o := h.api.Object(web.path())
	o["metadata"].(map[string]any)["annotations"] = map[string]any{prefix + "replicas": "1"}
	o["spec"].(map[string]any)["replicas"] = 0
	h.api.Set(web.path(), o)
	h.reconcile()
	if h.replicas(web) != 1 {
		t.Fatal("always-on group not woken")
	}
}

func TestAPIFailureIsReported(t *testing.T) {
	h := newHarness(t, []config.Group{{Name: "a", Default: config.Asleep, Apps: []string{"a"}}})
	if err := h.c.Ready(); err == nil {
		t.Fatal("ready before the first reconcile")
	}
	h.api.Intercept = func(method, path string) (int, bool) { return 500, strings.Contains(path, "applications") }
	h.reconcile()
	if err := h.c.Ready(); err == nil || !strings.Contains(err.Error(), "did not reach the Kubernetes API") {
		t.Fatalf("Ready = %v", err)
	}
	if err := h.c.Live(); err != nil {
		t.Fatalf("Live = %v", err)
	}
	if h.c.Status().ErrorsTotal != 1 {
		t.Fatalf("errors = %d", h.c.Status().ErrorsTotal)
	}
	h.api.Intercept = nil
	h.reconcile()
	if err := h.c.Ready(); err != nil {
		t.Fatalf("Ready = %v", err)
	}
	h.clock.Add(10 * time.Minute)
	if err := h.c.Live(); err == nil {
		t.Fatal("live although the loop stopped")
	}
}

func TestRunAndKick(t *testing.T) {
	web := deploy("next", "web")
	h := newHarness(t, []config.Group{{Name: "nextcloud", Default: config.Asleep, Apps: []string{"nextcloud"}}})
	h.app("nextcloud", web)
	h.c.opts.Now = time.Now // real time for the loop
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { h.c.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, func() bool { return h.replicas(web) == 0 })
	if _, err := h.c.Mutate(ctx, "nextcloud", func(g *state.Group, now time.Time) error {
		g.Leases = map[string]state.Lease{"x": {Expires: now.Add(time.Hour)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Mutate kicks the loop: no need to wait for the interval.
	waitFor(t, func() bool { return h.replicas(web) == 2 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
