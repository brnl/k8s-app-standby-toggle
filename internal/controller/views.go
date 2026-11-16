package controller

import (
	"context"
	"slices"
	"time"

	"github.com/brnl/k8s-app-standby-toggle/internal/config"
	"github.com/brnl/k8s-app-standby-toggle/internal/standby"
	"github.com/brnl/k8s-app-standby-toggle/internal/state"
)

// GroupView is a group as the API returns it.
type GroupView struct {
	Name     string        `json:"name"`
	Apps     []string      `json:"apps"`
	Default  string        `json:"default"`
	Desired  string        `json:"desired"`
	Phase    string        `json:"phase"`
	Ready    bool          `json:"ready"`
	Reason   string        `json:"reason"`
	Source   string        `json:"source"`
	Override *OverrideView `json:"override"`
	Leases   []LeaseView   `json:"leases"`
	Hold     *HoldView     `json:"hold"`
	Conflict *ConflictView `json:"conflict"`
	// Missing lists configured Applications that do not exist.
	Missing   []string       `json:"missing"`
	Resources []ResourceView `json:"resources"`
	Errors    []string       `json:"errors,omitempty"`
	Observed  *time.Time     `json:"observed,omitempty"`
}

// OverrideView is a person's override.
type OverrideView struct {
	State   string     `json:"state"`
	Expires *time.Time `json:"expires"`
	By      string     `json:"by"`
	Set     time.Time  `json:"set"`
}

// LeaseView is a machine's lease.
type LeaseView struct {
	Owner   string    `json:"owner"`
	By      string    `json:"by"`
	Expires time.Time `json:"expires"`
	Created time.Time `json:"created"`
}

// HoldView is a hold.
type HoldView struct {
	Reason  string    `json:"reason"`
	By      string    `json:"by"`
	Expires time.Time `json:"expires"`
	Set     time.Time `json:"set"`
}

// ConflictView says why the service stopped acting on a group.
type ConflictView struct {
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// ResourceView is one resource of a group.
type ResourceView struct {
	Application string `json:"application"`
	Kind        string `json:"kind"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Replicas    int32  `json:"replicas"`
	Ready       int32  `json:"ready"`
	Suspended   *bool  `json:"suspended,omitempty"`
	// State is awake, asleep, changing or skipped.
	State   string `json:"state"`
	Skipped string `json:"skipped,omitempty"`
}

// Status is the loop's overall status.
type Status struct {
	Started     time.Time
	LastLoop    time.Time
	APIOK       bool
	LastError   string
	Reconciles  uint64
	ErrorsTotal uint64
}

// StateLoaded reports whether the stored state has been read at least once.
func (c *Controller) StateLoaded() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loaded
}

// Group returns the configuration of a group.
func (c *Controller) Group(name string) (config.Group, bool) {
	g, ok := c.groups[name]
	return g, ok
}

// Views returns every group, sorted by name.
func (c *Controller) Views() []GroupView {
	now := c.opts.Now()
	out := make([]GroupView, 0, len(c.order))
	for _, name := range c.order {
		out = append(out, c.view(name, now))
	}
	return out
}

// View returns one group.
func (c *Controller) View(name string) (GroupView, bool) {
	if _, ok := c.groups[name]; !ok {
		return GroupView{}, false
	}
	return c.view(name, c.opts.Now()), true
}

// Status returns the loop's status.
func (c *Controller) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Status{
		Started: c.started, LastLoop: c.lastLoop, APIOK: c.apiOK, LastError: c.lastErr,
		Reconciles: c.reconciles, ErrorsTotal: c.errorsTotal,
	}
}

func (c *Controller) view(name string, now time.Time) GroupView {
	g := c.groups[name]
	c.mu.RLock()
	gs := c.st.Get(name)
	o := c.obs[name]
	c.mu.RUnlock()

	eff := state.Evaluate(g.Default, gs, now)
	v := GroupView{
		Name:      g.Name,
		Apps:      slices.Clone(g.Apps),
		Default:   string(g.Default),
		Desired:   eff.State(),
		Phase:     PhaseUnknown,
		Reason:    eff.Reason,
		Source:    string(eff.Source),
		Leases:    []LeaseView{},
		Missing:   []string{},
		Resources: []ResourceView{},
	}
	if gs != nil {
		if ov := gs.Override; ov != nil && (ov.Expires == nil || ov.Expires.After(now)) {
			v.Override = &OverrideView{State: ov.State, Expires: ov.Expires, By: ov.By, Set: ov.Set}
		}
		for owner, l := range gs.Leases {
			if l.Expires.After(now) {
				v.Leases = append(v.Leases, LeaseView{Owner: owner, By: l.By, Expires: l.Expires, Created: l.Created})
			}
		}
		slices.SortFunc(v.Leases, func(a, b LeaseView) int { return a.Expires.Compare(b.Expires) })
		if h := gs.Hold; h != nil && h.Expires.After(now) {
			v.Hold = &HoldView{Reason: h.Reason, By: h.By, Expires: h.Expires, Set: h.Set}
		}
		if cf := gs.Conflict; cf != nil {
			v.Conflict = &ConflictView{Reason: cf.Reason, Since: cf.Since}
			v.Phase = PhaseConflict
		}
	}
	if o == nil {
		return v
	}
	at := o.at
	v.Observed = &at
	v.Missing = append(v.Missing, o.missing...)
	v.Errors = slices.Clone(o.errors)
	for _, s := range o.resources {
		v.Resources = append(v.Resources, resourceView(s))
	}
	switch {
	case v.Conflict != nil:
	case o.awake == eff.Awake && o.phase != PhaseConflict:
		v.Phase, v.Ready = o.phase, o.ready
	case eff.Awake:
		// Changed since the last pass; the next one starts waking it.
		v.Phase = PhaseWaking
	default:
		v.Phase = PhaseSleeping
	}
	return v
}

func resourceView(s standby.Status) ResourceView {
	r := ResourceView{
		Application: s.App, Kind: string(s.Kind), Namespace: s.Namespace, Name: s.Name,
		Replicas: s.Replicas, Ready: s.ReadyReplicas, Skipped: s.Skip,
	}
	if s.Kind == standby.CronJob {
		suspended := s.Suspended
		r.Suspended = &suspended
	}
	switch {
	case s.Skip != "":
		r.State = "skipped"
	case s.Asleep:
		r.State = "asleep"
	case s.Awake:
		r.State = "awake"
	default:
		r.State = "changing"
	}
	return r
}

// Mutate applies fn to a group's stored state, starts a reconcile and
// returns the updated group. fn receives the current time and may run more
// than once.
func (c *Controller) Mutate(ctx context.Context, name string, fn func(g *state.Group, now time.Time) error) (GroupView, error) {
	now := c.opts.Now()
	clearedConflict := false
	st, err := c.opts.Store.Update(ctx, func(s *state.State) error {
		g := s.Group(name)
		had := g.Conflict != nil
		if err := fn(g, now); err != nil {
			return err
		}
		clearedConflict = had && g.Conflict == nil
		return nil
	})
	if err != nil {
		return GroupView{}, err
	}
	c.mu.Lock()
	c.st, c.loaded = st, true
	if clearedConflict {
		delete(c.undos, name)
	}
	c.mu.Unlock()
	c.Kick()
	return c.view(name, now), nil
}
