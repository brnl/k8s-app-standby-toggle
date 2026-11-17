// Package standby puts individual Kubernetes resources to sleep and wakes
// them again, recording the previous value in an annotation on the object.
//
//	Kind                         Sleep                                 Wake
//	apps/Deployment, StatefulSet record spec.replicas, scale to 0        restore spec.replicas
//	postgresql.cnpg.io/Cluster   record cnpg.io/hibernation, set "on"  set "off" (or the recorded "on")
//	batch/CronJob                record spec.suspend, set true         restore spec.suspend
//
// Every operation is idempotent: the recorded value is written once, and an
// object without a recorded value is left alone on wake.
package standby

import (
	"context"
	"fmt"
	"strconv"

	"github.com/brnl/k8s-app-standby-toggle/internal/kube"
)

// Kind is a supported resource kind.
type Kind string

// Supported kinds.
const (
	Deployment  Kind = "Deployment"
	StatefulSet Kind = "StatefulSet"
	CronJob     Kind = "CronJob"
	Database    Kind = "Cluster" // postgresql.cnpg.io/Cluster
)

// Annotation names used by CloudNativePG.
const (
	CNPGHibernation = "cnpg.io/hibernation"
	cnpgHealthy     = "Cluster in healthy state"
)

// KindOf returns the supported kind for an API group and kind, or false.
func KindOf(group, kind string) (Kind, bool) {
	switch {
	case group == "apps" && kind == "Deployment":
		return Deployment, true
	case group == "apps" && kind == "StatefulSet":
		return StatefulSet, true
	case group == "batch" && kind == "CronJob":
		return CronJob, true
	case group == "postgresql.cnpg.io" && kind == "Cluster":
		return Database, true
	}
	return "", false
}

// Ref identifies one resource of an Application.
type Ref struct {
	App       string
	Kind      Kind
	Namespace string
	Name      string
}

func (r Ref) String() string { return fmt.Sprintf("%s %s/%s", r.Kind, r.Namespace, r.Name) }

// IsDatabase reports whether the resource is a database (woken first, put to
// sleep last).
func (r Ref) IsDatabase() bool { return r.Kind == Database }

func (r Ref) path() string {
	switch r.Kind {
	case Deployment:
		return kube.Path("apps", "v1", "deployments", r.Namespace, r.Name)
	case StatefulSet:
		return kube.Path("apps", "v1", "statefulsets", r.Namespace, r.Name)
	case CronJob:
		return kube.Path("batch", "v1", "cronjobs", r.Namespace, r.Name)
	default:
		return kube.Path("postgresql.cnpg.io", "v1", "clusters", r.Namespace, r.Name)
	}
}

// Status is what the actuator observed about one resource.
type Status struct {
	Ref
	// Replicas is the desired number of pods (spec.replicas, or
	// spec.instances for a database); 0 for a CronJob.
	Replicas int32
	// ReadyReplicas is the number of ready pods (instances for a database).
	ReadyReplicas int32
	// Suspended is set for a suspended CronJob.
	Suspended bool
	// Recorded is set while the object carries a recorded value, i.e. the
	// service put it to sleep and has not woken it.
	Recorded bool
	// Asleep: fully asleep (no pods left, hibernated, suspended).
	Asleep bool
	// Awake: fully awake and ready.
	Awake bool
	// Undone: the object was put to sleep by the service but was found
	// awake again.
	Undone bool
	// Skip is the reason the object is left alone, if it is.
	Skip string
	// Action describes the change made in this call, if any.
	Action string
}

// Actuator acts on resources.
type Actuator struct {
	client *kube.Client
	prefix string
}

// New returns an actuator that records previous values in annotations with
// the given prefix (which ends in "/").
func New(client *kube.Client, annotationPrefix string) *Actuator {
	return &Actuator{client: client, prefix: annotationPrefix}
}

// Annotation returns the full name of a recorded-value annotation.
func (a *Actuator) Annotation(key string) string { return a.prefix + key }

// object holds the fields of all supported kinds that the actuator reads.
type object struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		Replicas  *int32 `json:"replicas"`
		Suspend   *bool  `json:"suspend"`
		Instances *int32 `json:"instances"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64            `json:"observedGeneration"`
		Replicas           int32            `json:"replicas"`
		ReadyReplicas      int32            `json:"readyReplicas"`
		ReadyInstances     int32            `json:"readyInstances"`
		Phase              string           `json:"phase"`
		Conditions         []kube.Condition `json:"conditions"`
	} `json:"status"`
}

// Observe reads the resource without changing it.
func (a *Actuator) Observe(ctx context.Context, ref Ref) (Status, error) {
	o, err := a.get(ctx, ref)
	if err != nil {
		return Status{Ref: ref}, err
	}
	return a.status(ref, o), nil
}

// Sleep puts the resource to sleep.
func (a *Actuator) Sleep(ctx context.Context, ref Ref) (Status, error) {
	o, err := a.get(ctx, ref)
	if err != nil {
		return Status{Ref: ref}, err
	}
	if st := a.status(ref, o); st.Skip != "" {
		return st, nil
	}
	patch, action, undone := a.sleepPatch(ref, o)
	if patch == nil {
		return a.status(ref, o), nil
	}
	return a.apply(ctx, ref, o, patch, action, undone)
}

// Wake wakes the resource if the service put it to sleep.
func (a *Actuator) Wake(ctx context.Context, ref Ref) (Status, error) {
	o, err := a.get(ctx, ref)
	if err != nil {
		return Status{Ref: ref}, err
	}
	if st := a.status(ref, o); st.Skip != "" {
		return st, nil
	}
	patch, action, err := a.wakePatch(ref, o)
	if err != nil {
		st := a.status(ref, o)
		return st, err
	}
	if patch == nil {
		return a.status(ref, o), nil
	}
	return a.apply(ctx, ref, o, patch, action, false)
}

func (a *Actuator) get(ctx context.Context, ref Ref) (*object, error) {
	var o object
	if err := a.client.Get(ctx, ref.path(), &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// apply sends a merge patch that only succeeds if the object did not change
// since it was read.
func (a *Actuator) apply(ctx context.Context, ref Ref, o *object, patch map[string]any, action string, undone bool) (Status, error) {
	meta, _ := patch["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		patch["metadata"] = meta
	}
	meta["resourceVersion"] = o.Metadata.ResourceVersion
	var updated object
	if err := a.client.Patch(ctx, ref.path(), patch, &updated); err != nil {
		st := a.status(ref, o)
		return st, err
	}
	st := a.status(ref, &updated)
	st.Action, st.Undone = action, undone
	return st, nil
}

func (a *Actuator) sleepPatch(ref Ref, o *object) (patch map[string]any, action string, undone bool) {
	ann := o.Metadata.Annotations
	switch ref.Kind {
	case Deployment, StatefulSet:
		cur := replicas(o)
		key := a.Annotation("replicas")
		if _, recorded := ann[key]; !recorded {
			return map[string]any{
				"metadata": map[string]any{"annotations": map[string]any{key: strconv.Itoa(int(cur))}},
				"spec":     map[string]any{"replicas": 0},
			}, fmt.Sprintf("scaled to 0 (recorded %d replicas)", cur), false
		}
		if cur > 0 {
			return map[string]any{"spec": map[string]any{"replicas": 0}},
				fmt.Sprintf("scaled back to 0 (something set it to %d)", cur), true
		}
	case CronJob:
		cur := o.Spec.Suspend != nil && *o.Spec.Suspend
		key := a.Annotation("suspend")
		if _, recorded := ann[key]; !recorded {
			return map[string]any{
				"metadata": map[string]any{"annotations": map[string]any{key: strconv.FormatBool(cur)}},
				"spec":     map[string]any{"suspend": true},
			}, fmt.Sprintf("suspended (recorded suspend=%t)", cur), false
		}
		if !cur {
			return map[string]any{"spec": map[string]any{"suspend": true}},
				"suspended again (something resumed it)", true
		}
	case Database:
		cur := ann[CNPGHibernation]
		key := a.Annotation("hibernation")
		if _, recorded := ann[key]; !recorded {
			prev := "off"
			if cur == "on" {
				prev = "on"
			}
			return map[string]any{
				"metadata": map[string]any{"annotations": map[string]any{key: prev, CNPGHibernation: "on"}},
			}, fmt.Sprintf("hibernated (recorded %s=%s)", CNPGHibernation, prev), false
		}
		if cur != "on" {
			return map[string]any{
				"metadata": map[string]any{"annotations": map[string]any{CNPGHibernation: "on"}},
			}, "hibernated again (something turned hibernation off)", true
		}
	}
	return nil, "", false
}

func (a *Actuator) wakePatch(ref Ref, o *object) (patch map[string]any, action string, err error) {
	ann := o.Metadata.Annotations
	switch ref.Kind {
	case Deployment, StatefulSet:
		key := a.Annotation("replicas")
		v, recorded := ann[key]
		if !recorded {
			return nil, "", nil
		}
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 0 {
			return nil, "", fmt.Errorf("%s: annotation %s=%q is not a replica count; restore spec.replicas by hand and remove the annotation", ref, key, v)
		}
		return map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{key: nil}},
			"spec":     map[string]any{"replicas": n},
		}, fmt.Sprintf("scaled to %d (restored)", n), nil
	case CronJob:
		key := a.Annotation("suspend")
		v, recorded := ann[key]
		if !recorded {
			return nil, "", nil
		}
		prev, err := strconv.ParseBool(v)
		if err != nil {
			return nil, "", fmt.Errorf("%s: annotation %s=%q is not a boolean; restore spec.suspend by hand and remove the annotation", ref, key, v)
		}
		return map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{key: nil}},
			"spec":     map[string]any{"suspend": prev},
		}, fmt.Sprintf("suspend=%t (restored)", prev), nil
	case Database:
		key := a.Annotation("hibernation")
		v, recorded := ann[key]
		if !recorded {
			return nil, "", nil
		}
		next := "off"
		if v == "on" {
			next = "on" // it was hibernated before the service touched it
		}
		return map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{key: nil, CNPGHibernation: next}},
		}, fmt.Sprintf("%s=%s (restored)", CNPGHibernation, next), nil
	}
	return nil, "", nil
}

// status derives the observed state of an object.
func (a *Actuator) status(ref Ref, o *object) Status {
	st := Status{Ref: ref}
	if len(o.Metadata.OwnerReferences) > 0 {
		owner := o.Metadata.OwnerReferences[0]
		st.Skip = fmt.Sprintf("owned by %s %s", owner.Kind, owner.Name)
		return st
	}
	ann := o.Metadata.Annotations
	switch ref.Kind {
	case Deployment, StatefulSet:
		st.Replicas = replicas(o)
		st.ReadyReplicas = o.Status.ReadyReplicas
		_, st.Recorded = ann[a.Annotation("replicas")]
		current := o.Status.ObservedGeneration >= o.Metadata.Generation
		st.Asleep = st.Recorded && st.Replicas == 0 && current && o.Status.Replicas == 0
		st.Awake = !st.Recorded && current && o.Status.ReadyReplicas == st.Replicas
	case CronJob:
		st.Suspended = o.Spec.Suspend != nil && *o.Spec.Suspend
		_, st.Recorded = ann[a.Annotation("suspend")]
		st.Asleep = st.Recorded && st.Suspended
		st.Awake = !st.Recorded
	case Database:
		st.Replicas = 1
		if o.Spec.Instances != nil {
			st.Replicas = *o.Spec.Instances
		}
		st.ReadyReplicas = o.Status.ReadyInstances
		_, st.Recorded = ann[a.Annotation("hibernation")]
		hibernated := false
		for _, c := range o.Status.Conditions {
			if c.Type == CNPGHibernation && c.Status == "True" {
				hibernated = true
			}
		}
		st.Asleep = st.Recorded && ann[CNPGHibernation] == "on" && hibernated
		// A hibernated cluster can still report the healthy phase, so the
		// hibernation annotation and condition are checked as well.
		st.Awake = !st.Recorded && ann[CNPGHibernation] != "on" && !hibernated &&
			o.Status.Phase == cnpgHealthy && o.Status.ReadyInstances >= st.Replicas
	}
	return st
}

// replicas returns spec.replicas, which defaults to 1.
func replicas(o *object) int32 {
	if o.Spec.Replicas == nil {
		return 1
	}
	return *o.Spec.Replicas
}
