// Package controller runs the reconcile loop: it evaluates each group's
// effective state, brings the group's resources to that state in the right
// order, and records what it observed.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/brnl/k8s-app-standby-toggle/internal/config"
	"github.com/brnl/k8s-app-standby-toggle/internal/kube"
	"github.com/brnl/k8s-app-standby-toggle/internal/standby"
	"github.com/brnl/k8s-app-standby-toggle/internal/state"
)

// Observed phases of a group.
const (
	PhaseUnknown  = "unknown"
	PhaseAwake    = "awake"
	PhaseWaking   = "waking"
	PhaseAsleep   = "asleep"
	PhaseSleeping = "sleeping"
	PhaseDegraded = "degraded"
	PhaseConflict = "conflict"
)

// Phases lists every phase, for metrics.
var Phases = []string{PhaseUnknown, PhaseAwake, PhaseWaking, PhaseAsleep, PhaseSleeping, PhaseDegraded, PhaseConflict}

// reconcileTimeout bounds one reconcile pass.
const reconcileTimeout = 2 * time.Minute

// Options configures a Controller.
type Options struct {
	Groups          []config.Group
	ArgoCDNamespace string
	// Namespace is the service's own namespace.
	Namespace string
	Client    *kube.Client
	Store     state.Store
	Actuator  *standby.Actuator

	Interval time.Duration
	// FastInterval is used while a group is waking or sleeping.
	FastInterval      time.Duration
	DegradedAfter     time.Duration
	DatabaseWait      time.Duration
	ConflictThreshold int
	ConflictWindow    time.Duration

	Now func() time.Time
	Log *slog.Logger
}

// Controller reconciles groups and serves their state.
type Controller struct {
	opts       Options
	groups     map[string]config.Group
	order      []string
	configured map[string]bool
	protected  map[string]bool
	kick       chan struct{}
	started    time.Time

	mu          sync.RWMutex
	st          *state.State
	loaded      bool // st has been read from the store
	obs         map[string]*observation
	undos       map[string][]time.Time
	lastLoop    time.Time
	apiOK       bool // the last pass reached the Kubernetes API
	lastErr     string
	reconciles  uint64
	errorsTotal uint64

	// Used by the loop goroutine only.
	desired map[string]track
	logged  map[string]bool
}

// track follows a group's effective state over time.
type track struct {
	awake bool
	since time.Time
}

type observation struct {
	awake     bool // the effective state the observation was made for
	phase     string
	ready     bool
	resources []standby.Status
	missing   []string
	errors    []string
	at        time.Time
}

// New returns a controller.
func New(opts Options) *Controller {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.FastInterval <= 0 || opts.FastInterval > opts.Interval {
		opts.FastInterval = min(5*time.Second, opts.Interval)
	}
	c := &Controller{
		opts:       opts,
		groups:     map[string]config.Group{},
		configured: map[string]bool{},
		protected: map[string]bool{
			"kube-system": true, "kube-public": true, "kube-node-lease": true,
			opts.ArgoCDNamespace: true, opts.Namespace: true,
		},
		kick:    make(chan struct{}, 1),
		started: opts.Now(),
		st:      state.New(),
		obs:     map[string]*observation{},
		undos:   map[string][]time.Time{},
		desired: map[string]track{},
		logged:  map[string]bool{},
	}
	for _, g := range opts.Groups {
		c.groups[g.Name] = g
		c.order = append(c.order, g.Name)
		c.configured[g.Name] = true
	}
	sort.Strings(c.order)
	return c
}

// Kick asks for a reconcile as soon as possible.
func (c *Controller) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) {
	for {
		busy := c.Reconcile(ctx)
		wait := c.opts.Interval
		if busy {
			wait = c.opts.FastInterval
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-c.kick:
			t.Stop()
		}
	}
}

// Reconcile runs one pass over all groups. It reports whether a group is
// still changing, so the next pass should come sooner.
func (c *Controller) Reconcile(ctx context.Context) (busy bool) {
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	now := c.opts.Now()
	log := c.opts.Log

	var removed []state.Removed
	st, err := c.opts.Store.Update(ctx, func(s *state.State) error {
		removed = append(s.DropExpired(now), s.Prune(c.configured)...)
		return nil
	})
	if err != nil {
		c.finish(fmt.Errorf("loading state: %w", err), 1, false)
		return false
	}
	for _, r := range removed {
		log.Info("state changed: "+r.Detail, "group", r.Group, "item", r.Item, "by", "expiry", "set_by", r.By)
	}
	c.mu.Lock()
	c.st, c.loaded = st, true
	c.mu.Unlock()

	var apps kube.ApplicationList
	if err := c.opts.Client.Get(ctx, kube.Path("argoproj.io", "v1alpha1", "applications", c.opts.ArgoCDNamespace, ""), &apps); err != nil {
		c.finish(fmt.Errorf("listing Argo CD Applications: %w", err), 1, false)
		return false
	}
	byName := make(map[string]*kube.Application, len(apps.Items))
	for i := range apps.Items {
		byName[apps.Items[i].Metadata.Name] = &apps.Items[i]
	}

	errCount := 0
	for _, name := range c.order {
		o := c.reconcileGroup(ctx, c.groups[name], st, byName, now)
		errCount += len(o.errors)
		c.mu.Lock()
		prev := c.obs[name]
		c.obs[name] = o
		c.mu.Unlock()
		if prev == nil || prev.phase != o.phase {
			from := PhaseUnknown
			if prev != nil {
				from = prev.phase
			}
			log.Info("group phase changed", "group", name, "from", from, "to", o.phase, "ready", o.ready)
		}
		if o.phase == PhaseWaking || o.phase == PhaseSleeping {
			busy = true
		}
	}
	var loopErr error
	if errCount > 0 {
		loopErr = fmt.Errorf("%d resource error(s); see the groups for details", errCount)
	}
	c.finish(loopErr, errCount, true)
	return busy
}

// finish records the end of a pass. A pass that listed the Applications
// reached the Kubernetes API, even if some resources failed.
func (c *Controller) finish(err error, errCount int, reachedAPI bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastLoop = c.opts.Now()
	c.reconciles++
	c.errorsTotal += uint64(errCount)
	c.apiOK = reachedAPI
	if err != nil {
		if c.lastErr != err.Error() {
			c.opts.Log.Error("reconcile failed", "error", err.Error())
		}
		c.lastErr = err.Error()
		return
	}
	c.lastErr = ""
}

func (c *Controller) reconcileGroup(ctx context.Context, g config.Group, st *state.State, apps map[string]*kube.Application, now time.Time) *observation {
	gs := st.Get(g.Name)
	eff := state.Evaluate(g.Default, gs, now)
	t := c.trackDesired(g, eff, now)
	o := &observation{awake: eff.Awake, at: now}

	refs, missing := c.collect(g, apps)
	o.missing = missing
	var dbs, rest []standby.Ref
	for _, r := range refs {
		if r.IsDatabase() {
			dbs = append(dbs, r)
		} else {
			rest = append(rest, r)
		}
	}

	if gs != nil && gs.Conflict != nil {
		for _, r := range refs {
			o.add(c.do(ctx, g, eff, r, c.opts.Actuator.Observe))
		}
		o.phase = PhaseConflict
		return o
	}

	waited := now.Sub(t.since)
	if eff.Awake {
		// Databases first; the rest once they are ready (bounded).
		dbReady := true
		for _, r := range dbs {
			s := o.add(c.do(ctx, g, eff, r, c.opts.Actuator.Wake))
			dbReady = dbReady && (s.Awake || s.Skip != "")
		}
		act := c.opts.Actuator.Observe
		switch {
		case dbReady:
			act = c.opts.Actuator.Wake
		case waited >= c.opts.DatabaseWait:
			c.logOnce(fmt.Sprintf("dbwait/%s/%d", g.Name, t.since.UnixNano()), slog.LevelWarn,
				"databases not ready in time; waking the rest of the group anyway", "group", g.Name, "waited", c.opts.DatabaseWait.String())
			act = c.opts.Actuator.Wake
		}
		for _, r := range rest {
			o.add(c.do(ctx, g, eff, r, act))
		}
	} else {
		// The rest first; databases once its pods are gone (bounded).
		restAsleep := true
		for _, r := range rest {
			s := o.add(c.do(ctx, g, eff, r, c.opts.Actuator.Sleep))
			restAsleep = restAsleep && (s.Asleep || s.Skip != "")
		}
		act := c.opts.Actuator.Observe
		if restAsleep || waited >= c.opts.DatabaseWait {
			act = c.opts.Actuator.Sleep
		}
		for _, r := range dbs {
			o.add(c.do(ctx, g, eff, r, act))
		}
	}

	for _, s := range o.resources {
		if s.Undone {
			if reason := c.recordUndo(g.Name, s, now); reason != "" {
				c.setConflict(ctx, g.Name, reason, now)
				o.phase = PhaseConflict
				return o
			}
		}
	}
	o.phase, o.ready = phaseOf(eff.Awake, o.resources, o.errors, waited, c.opts.DegradedAfter)
	return o
}

func (o *observation) add(s standby.Status, err error) standby.Status {
	if err != nil {
		o.errors = append(o.errors, err.Error())
	}
	o.resources = append(o.resources, s)
	return s
}

// do runs one action on a resource and logs what changed.
func (c *Controller) do(ctx context.Context, g config.Group, eff state.Effective, r standby.Ref, act func(context.Context, standby.Ref) (standby.Status, error)) (standby.Status, error) {
	s, err := act(ctx, r)
	switch {
	case kube.IsNotFound(err):
		// Listed by Argo CD but gone; the Application status catches up.
		c.opts.Log.Debug("resource not found", "group", g.Name, "app", r.App, "resource", r.String())
		return standby.Status{Ref: r, Skip: "not found"}, nil
	case kube.IsConflict(err):
		// Changed between read and patch; the next pass retries.
		c.opts.Log.Debug("resource changed concurrently, retrying", "group", g.Name, "resource", r.String())
		return s, nil
	case err != nil:
		c.opts.Log.Error("resource action failed", "group", g.Name, "app", r.App, "resource", r.String(), "error", err.Error())
		return s, err
	}
	if s.Skip != "" {
		c.logOnce("skip/"+r.String(), slog.LevelInfo, "skipping resource", "group", g.Name, "app", r.App, "resource", r.String(), "reason", s.Skip)
	}
	if s.Action != "" {
		level := slog.LevelInfo
		if s.Undone {
			level = slog.LevelWarn
		}
		c.opts.Log.Log(ctx, level, "resource "+eff.State()+": "+s.Action,
			"group", g.Name, "app", r.App, "kind", string(r.Kind), "namespace", r.Namespace, "name", r.Name, "cause", eff.Reason)
	}
	return s, nil
}

// collect returns the supported resources of a group's Applications, and
// the Applications that do not exist.
func (c *Controller) collect(g config.Group, apps map[string]*kube.Application) (refs []standby.Ref, missing []string) {
	seen := map[string]bool{}
	for _, name := range g.Apps {
		app, ok := apps[name]
		if !ok {
			missing = append(missing, name)
			c.logOnce("missing/"+name, slog.LevelWarn, "Application not found", "group", g.Name, "app", name, "namespace", c.opts.ArgoCDNamespace)
			continue
		}
		delete(c.logged, "missing/"+name)
		for _, res := range app.Status.Resources {
			kind, ok := standby.KindOf(res.Group, res.Kind)
			if !ok {
				if res.Kind == "DaemonSet" {
					c.logOnce(fmt.Sprintf("skip/DaemonSet %s/%s", res.Namespace, res.Name), slog.LevelInfo, "skipping resource",
						"group", g.Name, "app", name, "resource", fmt.Sprintf("DaemonSet %s/%s", res.Namespace, res.Name), "reason", "DaemonSets are never put to sleep")
				}
				continue
			}
			r := standby.Ref{App: name, Kind: kind, Namespace: res.Namespace, Name: res.Name}
			if res.Namespace == "" || c.protected[res.Namespace] {
				c.logOnce("skip/"+r.String(), slog.LevelInfo, "skipping resource",
					"group", g.Name, "app", name, "resource", r.String(), "reason", "protected namespace")
				continue
			}
			if key := r.String(); !seen[key] {
				seen[key] = true
				refs = append(refs, r)
			}
		}
	}
	return refs, missing
}

// phaseOf derives a group's phase from its resources.
func phaseOf(awake bool, resources []standby.Status, errs []string, waited, degradedAfter time.Duration) (string, bool) {
	done := len(errs) == 0
	for _, s := range resources {
		if s.Skip != "" {
			continue
		}
		if awake && !s.Awake || !awake && !s.Asleep {
			done = false
		}
	}
	switch {
	case awake && done:
		return PhaseAwake, true
	case awake && waited >= degradedAfter:
		return PhaseDegraded, false
	case awake:
		return PhaseWaking, false
	case done:
		return PhaseAsleep, false
	default:
		return PhaseSleeping, false
	}
}

// trackDesired records when a group's effective state last changed.
func (c *Controller) trackDesired(g config.Group, eff state.Effective, now time.Time) track {
	t, ok := c.desired[g.Name]
	switch {
	case !ok:
		t = track{awake: eff.Awake, since: c.started}
		c.opts.Log.Info("group state", "group", g.Name, "default", string(g.Default), "desired", eff.State(), "reason", eff.Reason)
	case t.awake != eff.Awake:
		from := state.StateAsleep
		if t.awake {
			from = state.StateAwake
		}
		t = track{awake: eff.Awake, since: now}
		c.opts.Log.Info("group desired state changed", "group", g.Name, "from", from, "to", eff.State(), "source", string(eff.Source), "reason", eff.Reason)
	}
	c.desired[g.Name] = t
	return t
}

// recordUndo notes that a resource was found awake after the service put it
// to sleep. It returns a conflict reason once that happened
// ConflictThreshold times within ConflictWindow.
func (c *Controller) recordUndo(group string, s standby.Status, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := now.Add(-c.opts.ConflictWindow)
	var recent []time.Time
	for _, t := range c.undos[group] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	c.undos[group] = recent
	if len(recent) < c.opts.ConflictThreshold {
		return ""
	}
	return fmt.Sprintf("%s was woken by something else %d times within %s (last: %s). Another controller is undoing the sleep: Argo CD without ignoreDifferences on /spec/replicas and RespectIgnoreDifferences=true, or an autoscaler. The service stopped acting on this group; fix the cause, then clear the conflict.",
		s.Ref.String(), len(recent), c.opts.ConflictWindow, s.Action)
}

func (c *Controller) setConflict(ctx context.Context, group, reason string, now time.Time) {
	c.opts.Log.Error("conflict: stopped acting on group", "group", group, "reason", reason, "by", "controller")
	st, err := c.opts.Store.Update(ctx, func(s *state.State) error {
		g := s.Group(group)
		if g.Conflict == nil {
			g.Conflict = &state.Conflict{Reason: reason, Since: now}
		}
		return nil
	})
	if err != nil {
		c.opts.Log.Error("recording conflict failed", "group", group, "error", err.Error())
		return
	}
	c.mu.Lock()
	c.st = st
	c.mu.Unlock()
}

func (c *Controller) logOnce(key string, level slog.Level, msg string, args ...any) {
	if c.logged[key] {
		return
	}
	c.logged[key] = true
	c.opts.Log.Log(context.Background(), level, msg, args...)
}

// Live reports whether the loop is still running.
func (c *Controller) Live() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	last := c.lastLoop
	if last.IsZero() {
		last = c.started
	}
	if age := c.opts.Now().Sub(last); age > 3*c.opts.Interval+reconcileTimeout {
		return fmt.Errorf("no reconcile completed for %s", age.Round(time.Second))
	}
	return nil
}

// Ready reports whether the last reconcile reached the Kubernetes API.
func (c *Controller) Ready() error {
	if err := c.Live(); err != nil {
		return err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch {
	case c.lastLoop.IsZero():
		return errors.New("no reconcile has completed yet")
	case !c.apiOK:
		return fmt.Errorf("the last reconcile did not reach the Kubernetes API: %s", c.lastErr)
	}
	return nil
}
