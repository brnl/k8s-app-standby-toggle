package standby

import (
	"context"
	"testing"

	"github.com/brnl/k8s-app-standby-toggle/internal/kube/kubetest"
)

const prefix = "k8s-app-standby-toggle.cloud64.net/"

var ctx = context.Background()

func workload(name string, replicas any, ann map[string]any) map[string]any {
	spec := map[string]any{}
	if replicas != nil {
		spec["replicas"] = replicas
	}
	meta := map[string]any{"name": name, "namespace": "app", "generation": 1}
	if ann != nil {
		meta["annotations"] = ann
	}
	status := map[string]any{"observedGeneration": 1}
	if n, ok := replicas.(int); ok {
		status["replicas"], status["readyReplicas"] = n, n
	}
	return map[string]any{"metadata": meta, "spec": spec, "status": status}
}

func annotations(o map[string]any) map[string]any {
	a, _ := o["metadata"].(map[string]any)["annotations"].(map[string]any)
	return a
}

func spec(o map[string]any, key string) any { return o["spec"].(map[string]any)[key] }

func TestWorkloadSleepWake(t *testing.T) {
	for _, kind := range []Kind{Deployment, StatefulSet} {
		t.Run(string(kind), func(t *testing.T) {
			api := kubetest.New(t)
			a := New(api.Client(), prefix)
			ref := Ref{App: "app", Kind: kind, Namespace: "app", Name: "web"}
			api.Set(ref.path(), workload("web", 3, nil))

			st, err := a.Sleep(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			o := api.Object(ref.path())
			if spec(o, "replicas") != 0.0 || annotations(o)[prefix+"replicas"] != "3" {
				t.Fatalf("after sleep: %v", o)
			}
			if st.Action == "" || !st.Recorded || st.Asleep {
				// status.replicas is still 3 until the controller catches up.
				t.Fatalf("status = %+v", st)
			}

			// Idempotent: a second sleep changes nothing and keeps the recorded value.
			api.ResetRequests()
			if st, err = a.Sleep(ctx, ref); err != nil || st.Action != "" {
				t.Fatalf("second sleep: %+v %v", st, err)
			}
			if n := api.Count("PATCH", ref.path()); n != 0 {
				t.Fatalf("%d patches for an idempotent sleep", n)
			}

			// The pods are gone: asleep.
			o = api.Object(ref.path())
			o["status"] = map[string]any{"observedGeneration": 1, "replicas": 0}
			api.Set(ref.path(), o)
			if st, _ = a.Observe(ctx, ref); !st.Asleep {
				t.Fatalf("not asleep: %+v", st)
			}

			if _, err = a.Wake(ctx, ref); err != nil {
				t.Fatal(err)
			}
			o = api.Object(ref.path())
			if spec(o, "replicas") != 3.0 {
				t.Fatalf("replicas after wake = %v", spec(o, "replicas"))
			}
			if _, ok := annotations(o)[prefix+"replicas"]; ok {
				t.Fatal("annotation kept after wake")
			}

			// Ready once the controller observed the generation and all pods are ready.
			o["status"] = map[string]any{"observedGeneration": 1, "replicas": 3, "readyReplicas": 3}
			api.Set(ref.path(), o)
			if st, _ = a.Observe(ctx, ref); !st.Awake || st.ReadyReplicas != 3 {
				t.Fatalf("not awake: %+v", st)
			}
			o["metadata"].(map[string]any)["generation"] = 2
			api.Set(ref.path(), o)
			if st, _ = a.Observe(ctx, ref); st.Awake {
				t.Fatal("awake before the controller observed the new generation")
			}
		})
	}
}

func TestWorkloadAtZeroStaysAtZero(t *testing.T) {
	api := kubetest.New(t)
	a := New(api.Client(), prefix)
	ref := Ref{Kind: Deployment, Namespace: "app", Name: "worker"}
	api.Set(ref.path(), workload("worker", 0, nil))

	if _, err := a.Sleep(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if v := annotations(api.Object(ref.path()))[prefix+"replicas"]; v != "0" {
		t.Fatalf("recorded %v, want 0", v)
	}
	if _, err := a.Wake(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if r := spec(api.Object(ref.path()), "replicas"); r != 0.0 {
		t.Fatalf("replicas after wake = %v, want 0", r)
	}
}

func TestWorkloadDefaultReplicas(t *testing.T) {
	api := kubetest.New(t)
	a := New(api.Client(), prefix)
	ref := Ref{Kind: Deployment, Namespace: "app", Name: "web"}
	api.Set(ref.path(), workload("web", nil, nil)) // spec.replicas unset means 1
	if _, err := a.Sleep(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if v := annotations(api.Object(ref.path()))[prefix+"replicas"]; v != "1" {
		t.Fatalf("recorded %v, want 1", v)
	}
}

func TestWakeWithoutAnnotationLeavesObjectAlone(t *testing.T) {
	api := kubetest.New(t)
	a := New(api.Client(), prefix)
	refs := map[Ref]map[string]any{
		{Kind: Deployment, Namespace: "app", Name: "d"}:  workload("d", 0, nil),
		{Kind: CronJob, Namespace: "app", Name: "c"}:     {"metadata": map[string]any{"name": "c"}, "spec": map[string]any{"suspend": true}},
		{Kind: Database, Namespace: "app", Name: "db"}:   {"metadata": map[string]any{"name": "db", "annotations": map[string]any{CNPGHibernation: "on"}}},
		{Kind: StatefulSet, Namespace: "app", Name: "s"}: workload("s", 2, nil),
	}
	for ref, obj := range refs {
		api.Set(ref.path(), obj)
	}
	api.ResetRequests()
	for ref := range refs {
		if st, err := a.Wake(ctx, ref); err != nil || st.Action != "" {
			t.Fatalf("%s: %+v %v", ref, st, err)
		}
	}
	if n := api.Count("PATCH", "/"); n != 0 {
		t.Fatalf("%d patches", n)
	}
}

func TestUndoneSleepIsDetected(t *testing.T) {
	api := kubetest.New(t)
	a := New(api.Client(), prefix)
	ref := Ref{Kind: Deployment, Namespace: "app", Name: "web"}
	api.Set(ref.path(), workload("web", 2, nil))
	if _, err := a.Sleep(ctx, ref); err != nil {
		t.Fatal(err)
	}
	// Something (Argo CD self-heal) scales it back up, keeping the annotation.
	o := api.Object(ref.path())
	o["spec"].(map[string]any)["replicas"] = 2
	api.Set(ref.path(), o)

	st, err := a.Sleep(ctx, ref)
	if err != nil || !st.Undone {
		t.Fatalf("undo not detected: %+v %v", st, err)
	}
	o = api.Object(ref.path())
	if spec(o, "replicas") != 0.0 || annotations(o)[prefix+"replicas"] != "2" {
		t.Fatalf("after re-sleep: %v", o)
	}
}

func TestCronJob(t *testing.T) {
	api := kubetest.New(t)
	a := New(api.Client(), prefix)
	ref := Ref{Kind: CronJob, Namespace: "app", Name: "backup"}
	api.Set(ref.path(), map[string]any{"metadata": map[string]any{"name": "backup"}, "spec": map[string]any{"schedule": "@daily"}})

	if _, err := a.Sleep(ctx, ref); err != nil {
		t.Fatal(err)
	}
	o := api.Object(ref.path())
	if spec(o, "suspend") != true || annotations(o)[prefix+"suspend"] != "false" {
		t.Fatalf("after sleep: %v", o)
	}
	if st, _ := a.Observe(ctx, ref); !st.Asleep || !st.Suspended {
		t.Fatalf("status: %+v", st)
	}
	if _, err := a.Wake(ctx, ref); err != nil {
		t.Fatal(err)
	}
	o = api.Object(ref.path())
	if spec(o, "suspend") != false {
		t.Fatalf("suspend after wake = %v", spec(o, "suspend"))
	}
	if st, _ := a.Observe(ctx, ref); !st.Awake {
		t.Fatalf("status: %+v", st)
	}

	// A CronJob that was suspended before stays suspended after waking.
	api.Set(ref.path(), map[string]any{"metadata": map[string]any{"name": "backup"}, "spec": map[string]any{"suspend": true}})
	_, _ = a.Sleep(ctx, ref)
	_, _ = a.Wake(ctx, ref)
	if spec(api.Object(ref.path()), "suspend") != true {
		t.Fatal("previously suspended CronJob was resumed")
	}
}

func TestDatabase(t *testing.T) {
	api := kubetest.New(t)
	a := New(api.Client(), prefix)
	ref := Ref{Kind: Database, Namespace: "app", Name: "db"}
	healthy := map[string]any{"phase": "Cluster in healthy state", "readyInstances": 2, "instances": 2}
	api.Set(ref.path(), map[string]any{"metadata": map[string]any{"name": "db"}, "spec": map[string]any{"instances": 2}, "status": healthy})

	if st, _ := a.Observe(ctx, ref); !st.Awake || st.Replicas != 2 {
		t.Fatalf("healthy cluster not awake: %+v", st)
	}
	if _, err := a.Sleep(ctx, ref); err != nil {
		t.Fatal(err)
	}
	o := api.Object(ref.path())
	if annotations(o)[CNPGHibernation] != "on" || annotations(o)[prefix+"hibernation"] != "off" {
		t.Fatalf("after sleep: %v", annotations(o))
	}
	// Asleep only once CNPG reports the hibernation condition.
	if st, _ := a.Observe(ctx, ref); st.Asleep {
		t.Fatal("asleep before the operator hibernated it")
	}
	o["status"] = map[string]any{
		"phase": "Cluster in healthy state", "readyInstances": 0, "instances": 2,
		"conditions": []any{map[string]any{"type": "cnpg.io/hibernation", "status": "True", "reason": "Hibernated"}},
	}
	api.Set(ref.path(), o)
	st, _ := a.Observe(ctx, ref)
	if !st.Asleep || st.Awake {
		t.Fatalf("hibernated cluster: %+v", st)
	}

	if _, err := a.Wake(ctx, ref); err != nil {
		t.Fatal(err)
	}
	o = api.Object(ref.path())
	if annotations(o)[CNPGHibernation] != "off" {
		t.Fatalf("after wake: %v", annotations(o))
	}
	if _, ok := annotations(o)[prefix+"hibernation"]; ok {
		t.Fatal("recorded value kept after wake")
	}
	// The stale hibernation condition (and the healthy phase it keeps)
	// must not count as ready.
	if st, _ := a.Observe(ctx, ref); st.Awake {
		t.Fatal("awake while still hibernated")
	}
	o["status"] = healthy
	api.Set(ref.path(), o)
	if st, _ := a.Observe(ctx, ref); !st.Awake {
		t.Fatalf("rehydrated cluster not awake: %+v", st)
	}
}

func TestOwnedObjectsAreSkipped(t *testing.T) {
	api := kubetest.New(t)
	a := New(api.Client(), prefix)
	ref := Ref{Kind: StatefulSet, Namespace: "monitoring", Name: "prometheus"}
	o := workload("prometheus", 1, nil)
	o["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "monitoring.coreos.com/v1", "kind": "Prometheus", "name": "k8s"}}
	api.Set(ref.path(), o)
	st, err := a.Sleep(ctx, ref)
	if err != nil || st.Skip != "owned by Prometheus k8s" {
		t.Fatalf("%+v %v", st, err)
	}
	if n := api.Count("PATCH", "/"); n != 0 {
		t.Fatal("patched an owned object")
	}
}

func TestPatchIsConditional(t *testing.T) {
	api := kubetest.New(t)
	a := New(api.Client(), prefix)
	ref := Ref{Kind: Deployment, Namespace: "app", Name: "web"}
	api.Set(ref.path(), workload("web", 2, nil))
	// The object changes between read and patch.
	api.Intercept = func(method, path string) (int, bool) {
		if method == "PATCH" {
			api.Intercept = nil
			api.Set(ref.path(), workload("web", 5, nil))
		}
		return 0, false
	}
	if _, err := a.Sleep(ctx, ref); err == nil {
		t.Fatal("patch on a stale object succeeded")
	}
	if spec(api.Object(ref.path()), "replicas") != 5.0 {
		t.Fatal("stale patch applied")
	}
}

func TestInvalidRecordedValue(t *testing.T) {
	api := kubetest.New(t)
	a := New(api.Client(), prefix)
	ref := Ref{Kind: Deployment, Namespace: "app", Name: "web"}
	api.Set(ref.path(), workload("web", 0, map[string]any{prefix + "replicas": "lots"}))
	if _, err := a.Wake(ctx, ref); err == nil {
		t.Fatal("invalid recorded value accepted")
	}
}

func TestKindOf(t *testing.T) {
	for _, tt := range []struct {
		group, kind string
		want        Kind
		ok          bool
	}{
		{"apps", "Deployment", Deployment, true},
		{"apps", "StatefulSet", StatefulSet, true},
		{"batch", "CronJob", CronJob, true},
		{"postgresql.cnpg.io", "Cluster", Database, true},
		{"apps", "DaemonSet", "", false},
		{"", "Service", "", false},
		{"other.io", "Cluster", "", false},
	} {
		if got, ok := KindOf(tt.group, tt.kind); got != tt.want || ok != tt.ok {
			t.Errorf("KindOf(%s, %s) = %s, %v", tt.group, tt.kind, got, ok)
		}
	}
}
