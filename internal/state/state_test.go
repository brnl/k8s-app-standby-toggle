package state

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/brnl/k8s-app-standby-toggle/internal/config"
	"github.com/brnl/k8s-app-standby-toggle/internal/kube"
	"github.com/brnl/k8s-app-standby-toggle/internal/kube/kubetest"
)

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time { t := t0.Add(d); return &t }

func TestEvaluateRules(t *testing.T) {
	future, past := t0.Add(time.Hour), t0.Add(-time.Minute)
	hold := &Hold{Reason: "test failed, run 123", By: "ci", Expires: future}
	lease := map[string]Lease{"ci-run-123": {By: "sa", Expires: future}}
	tests := []struct {
		name   string
		def    config.Default
		g      *Group
		awake  bool
		source Source
		reason string
	}{
		{"always-on wins over everything", config.AlwaysOn, &Group{Override: &Override{State: StateAsleep, By: "bram"}}, true, SourceAlwaysOn, "always on"},
		{"default asleep", config.Asleep, nil, false, SourceDefault, "asleep by default"},
		{"default awake", config.Awake, &Group{}, true, SourceDefault, "awake by default"},
		{"hold beats asleep override", config.Awake, &Group{Hold: hold, Override: &Override{State: StateAsleep, By: "bram"}}, true, SourceHold,
			"held until 2026-11-16T13:00:00Z by ci: test failed, run 123"},
		{"hold beats lease", config.Asleep, &Group{Hold: hold, Leases: lease}, true, SourceHold, ""},
		{"lease beats asleep override", config.Asleep, &Group{Leases: lease, Override: &Override{State: StateAsleep, By: "bram"}}, true, SourceLease,
			"lease ci-run-123 until 2026-11-16T13:00:00Z"},
		{"awake override", config.Asleep, &Group{Override: &Override{State: StateAwake, Expires: at(2 * time.Hour), By: "bram"}}, true, SourceOverride,
			"awake until 2026-11-16T14:00:00Z, woken by bram"},
		{"open-ended asleep override", config.Awake, &Group{Override: &Override{State: StateAsleep, By: "bram"}}, false, SourceOverride,
			"asleep, put to sleep by bram"},
		{"asleep override with expiry", config.Awake, &Group{Override: &Override{State: StateAsleep, Expires: at(time.Hour), By: "bram"}}, false, SourceOverride,
			"asleep until 2026-11-16T13:00:00Z, put to sleep by bram"},
		{"expired hold is ignored", config.Asleep, &Group{Hold: &Hold{Reason: "x", Expires: past}}, false, SourceDefault, ""},
		{"expired lease is ignored", config.Asleep, &Group{Leases: map[string]Lease{"a": {Expires: past}}}, false, SourceDefault, ""},
		{"expired override is ignored", config.Asleep, &Group{Override: &Override{State: StateAwake, Expires: &past}}, false, SourceDefault, ""},
		{"expiry is exclusive", config.Asleep, &Group{Override: &Override{State: StateAwake, Expires: at(0)}}, false, SourceDefault, ""},
		{"several leases", config.Asleep, &Group{Leases: map[string]Lease{
			"b": {Expires: t0.Add(2 * time.Hour)}, "a": {Expires: future}, "old": {Expires: past},
		}}, true, SourceLease, "leases a, b until 2026-11-16T14:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Evaluate(tt.def, tt.g, t0)
			if e.Awake != tt.awake || e.Source != tt.source {
				t.Fatalf("got awake=%v source=%s, want awake=%v source=%s", e.Awake, e.Source, tt.awake, tt.source)
			}
			if tt.reason != "" && e.Reason != tt.reason {
				t.Fatalf("reason = %q, want %q", e.Reason, tt.reason)
			}
		})
	}
}

func TestDropExpired(t *testing.T) {
	s := New()
	g := s.Group("vikunja")
	g.Override = &Override{State: StateAwake, Expires: at(time.Hour), By: "bram"}
	g.Leases = map[string]Lease{"run-1": {By: "ci", Expires: t0.Add(30 * time.Minute)}, "run-2": {By: "ci", Expires: t0.Add(2 * time.Hour)}}
	g.Hold = &Hold{Reason: "r", Expires: t0.Add(45 * time.Minute)}
	s.Group("open").Override = &Override{State: StateAsleep, By: "bram"}

	// Nothing has expired yet.
	if r := s.DropExpired(t0); len(r) != 0 {
		t.Fatalf("dropped %v", r)
	}
	// A fake clock walks past each expiry.
	if r := s.DropExpired(t0.Add(30 * time.Minute)); len(r) != 1 || r[0].Detail != "lease run-1 expired" {
		t.Fatalf("at 30m dropped %+v", r)
	}
	if r := s.DropExpired(t0.Add(time.Hour)); len(r) != 2 {
		t.Fatalf("at 1h dropped %+v", r)
	}
	if g.Override != nil || g.Hold != nil || len(g.Leases) != 1 {
		t.Fatalf("group after 1h: %+v", g)
	}
	if r := s.DropExpired(t0.Add(3 * time.Hour)); len(r) != 1 {
		t.Fatalf("at 3h dropped %+v", r)
	}
	if _, ok := s.Groups["vikunja"]; ok {
		t.Fatal("an empty group is kept")
	}
	// An open-ended override never expires.
	if s.Groups["open"] == nil || s.Groups["open"].Override == nil {
		t.Fatal("open-ended override dropped")
	}
}

func TestPrune(t *testing.T) {
	s := New()
	s.Group("old").Hold = &Hold{Expires: t0.Add(time.Hour)}
	s.Group("kept").Hold = &Hold{Expires: t0.Add(time.Hour)}
	if r := s.Prune(map[string]bool{"kept": true}); len(r) != 1 || r[0].Group != "old" {
		t.Fatalf("pruned %+v", r)
	}
	if s.Get("old") != nil || s.Get("kept") == nil {
		t.Fatal("wrong groups pruned")
	}
}

func TestMemoryStore(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()
	if _, err := m.Update(ctx, func(s *State) error { s.Group("a").Hold = &Hold{Reason: "x"}; return nil }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if _, err := m.Update(ctx, func(s *State) error { s.Group("a").Hold = nil; return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	s, _ := m.Load(ctx)
	if s.Get("a").Hold == nil {
		t.Fatal("a failed update changed the state")
	}
}

func TestConfigMapStore(t *testing.T) {
	api := kubetest.New(t)
	ctx := context.Background()
	store := NewConfigMapStore(api.Client(), "standby", "standby-state")
	path := kube.Path("", "v1", "configmaps", "standby", "standby-state")

	// Load creates the ConfigMap.
	s, err := store.Load(ctx)
	if err != nil || len(s.Groups) != 0 {
		t.Fatalf("Load = %+v, %v", s, err)
	}
	if api.Object(path) == nil {
		t.Fatal("ConfigMap not created")
	}

	// An update is stored as JSON.
	if _, err := store.Update(ctx, func(s *State) error {
		s.Group("vikunja").Leases = map[string]Lease{"run": {By: "ci", Expires: t0}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cm := api.Object(path)
	raw := cm["data"].(map[string]any)[ConfigMapKey].(string)
	var stored State
	if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored.Get("vikunja").Leases["run"].By != "ci" {
		t.Fatalf("stored %q: %v", raw, err)
	}

	// A concurrent change makes the first write fail; the update is retried
	// on top of the new state.
	calls := 0
	_, err = store.Update(ctx, func(s *State) error {
		calls++
		if calls == 1 {
			// Someone else writes in between.
			other := New()
			other.Group("other").Hold = &Hold{Reason: "concurrent"}
			obj := api.Object(path)
			obj["data"] = map[string]any{ConfigMapKey: encode(other)}
			api.Set(path, obj)
		}
		s.Group("vikunja").Hold = &Hold{Reason: "mine"}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	s, _ = store.Load(ctx)
	if s.Get("other") == nil || s.Get("vikunja").Hold.Reason != "mine" {
		t.Fatalf("lost a concurrent change: %+v", s.Groups)
	}

	// No write when nothing changed.
	api.ResetRequests()
	if _, err := store.Update(ctx, func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n := api.Count(http.MethodPut, path); n != 0 {
		t.Fatalf("%d writes for a no-op update", n)
	}

	// Corrupt state is reported, not overwritten.
	obj := api.Object(path)
	obj["data"] = map[string]any{ConfigMapKey: "{"}
	api.Set(path, obj)
	if _, err := store.Update(ctx, func(*State) error { return nil }); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("err = %v", err)
	}
}
