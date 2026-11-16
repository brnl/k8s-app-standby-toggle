// Package state holds the runtime deviations from each group's default
// (overrides, leases and holds), the rules that turn them into an effective
// state, and their storage.
package state

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/brnl/k8s-app-standby-toggle/internal/config"
)

// The two states a group can be in.
const (
	StateAwake  = "awake"
	StateAsleep = "asleep"
)

// Override is a person's choice to deviate from the default. An awake
// override always expires (auto-snooze); an asleep override may be
// open-ended (Expires is nil).
type Override struct {
	State   string     `json:"state"`
	Expires *time.Time `json:"expires,omitempty"`
	By      string     `json:"by"`
	Set     time.Time  `json:"set"`
}

// Lease keeps a group awake while a machine works with it.
type Lease struct {
	By      string    `json:"by"`
	Expires time.Time `json:"expires"`
	Created time.Time `json:"created"`
}

// Hold keeps a group awake for investigation.
type Hold struct {
	Reason  string    `json:"reason"`
	By      string    `json:"by"`
	Expires time.Time `json:"expires"`
	Set     time.Time `json:"set"`
}

// Conflict records that something else keeps undoing the service's work on
// a group, so the service stopped acting on it.
type Conflict struct {
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// Group holds the deviations of one group.
type Group struct {
	Override *Override        `json:"override,omitempty"`
	Leases   map[string]Lease `json:"leases,omitempty"`
	Hold     *Hold            `json:"hold,omitempty"`
	Conflict *Conflict        `json:"conflict,omitempty"`
}

func (g *Group) empty() bool {
	return g == nil || (g.Override == nil && len(g.Leases) == 0 && g.Hold == nil && g.Conflict == nil)
}

// State is everything the service stores at runtime.
type State struct {
	Groups map[string]*Group `json:"groups"`
}

// New returns an empty state.
func New() *State { return &State{Groups: map[string]*Group{}} }

// Group returns the state of the named group, creating it if needed.
func (s *State) Group(name string) *Group {
	if s.Groups == nil {
		s.Groups = map[string]*Group{}
	}
	g, ok := s.Groups[name]
	if !ok {
		g = &Group{}
		s.Groups[name] = g
	}
	return g
}

// Get returns the state of the named group, or nil.
func (s *State) Get(name string) *Group {
	if s == nil {
		return nil
	}
	return s.Groups[name]
}

// Clone returns a deep copy.
func (s *State) Clone() *State {
	c := New()
	b, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("state: encoding: %v", err))
	}
	if err := json.Unmarshal(b, c); err != nil {
		panic(fmt.Sprintf("state: decoding: %v", err))
	}
	if c.Groups == nil {
		c.Groups = map[string]*Group{}
	}
	return c
}

// Compact removes groups without any deviation.
func (s *State) Compact() {
	for name, g := range s.Groups {
		if len(g.Leases) == 0 {
			g.Leases = nil
		}
		if g.empty() {
			delete(s.Groups, name)
		}
	}
}

// Removed describes an item dropped from the state.
type Removed struct {
	Group  string
	Item   string // "override", "lease", "hold" or "group"
	Detail string
	By     string
}

// DropExpired deletes every override, lease and hold that expired at or
// before now, and returns what it removed.
func (s *State) DropExpired(now time.Time) []Removed {
	var removed []Removed
	for _, name := range s.names() {
		g := s.Groups[name]
		if o := g.Override; o != nil && o.Expires != nil && !o.Expires.After(now) {
			removed = append(removed, Removed{name, "override", o.State + " override expired", o.By})
			g.Override = nil
		}
		for _, owner := range sortedKeys(g.Leases) {
			if l := g.Leases[owner]; !l.Expires.After(now) {
				removed = append(removed, Removed{name, "lease", "lease " + owner + " expired", l.By})
				delete(g.Leases, owner)
			}
		}
		if h := g.Hold; h != nil && !h.Expires.After(now) {
			removed = append(removed, Removed{name, "hold", "hold expired", h.By})
			g.Hold = nil
		}
	}
	s.Compact()
	return removed
}

// Prune deletes the state of groups that are no longer configured.
func (s *State) Prune(configured map[string]bool) []Removed {
	var removed []Removed
	for _, name := range s.names() {
		if !configured[name] {
			removed = append(removed, Removed{Group: name, Item: "group", Detail: "group is no longer configured"})
			delete(s.Groups, name)
		}
	}
	return removed
}

func (s *State) names() []string {
	return sortedKeys(s.Groups)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Source says which rule decided the effective state.
type Source string

// The rules, in the order they are evaluated.
const (
	SourceAlwaysOn Source = "always-on"
	SourceHold     Source = "hold"
	SourceLease    Source = "lease"
	SourceOverride Source = "override"
	SourceDefault  Source = "default"
)

// Effective is a group's effective state and why.
type Effective struct {
	Awake  bool
	Source Source
	// Reason explains the state in one line, with RFC 3339 times.
	Reason string
}

// State returns "awake" or "asleep".
func (e Effective) State() string {
	if e.Awake {
		return StateAwake
	}
	return StateAsleep
}

// Evaluate returns the effective state of a group with default def and
// deviations g at time now. The first rule that matches wins:
//
//  1. always-on → awake
//  2. an unexpired hold → awake
//  3. an unexpired lease → awake
//  4. an unexpired override → its state
//  5. the default
func Evaluate(def config.Default, g *Group, now time.Time) Effective {
	if def == config.AlwaysOn {
		return Effective{Awake: true, Source: SourceAlwaysOn, Reason: "always on"}
	}
	if g != nil {
		if h := g.Hold; h != nil && h.Expires.After(now) {
			return Effective{Awake: true, Source: SourceHold,
				Reason: fmt.Sprintf("held until %s by %s: %s", stamp(h.Expires), h.By, h.Reason)}
		}
		if owners := activeLeases(g, now); len(owners) > 0 {
			latest := g.Leases[owners[0]].Expires
			for _, o := range owners[1:] {
				if e := g.Leases[o].Expires; e.After(latest) {
					latest = e
				}
			}
			reason := fmt.Sprintf("lease %s until %s", owners[0], stamp(latest))
			if len(owners) > 1 {
				reason = fmt.Sprintf("leases %s until %s", strings.Join(owners, ", "), stamp(latest))
			}
			return Effective{Awake: true, Source: SourceLease, Reason: reason}
		}
		if o := g.Override; o != nil && (o.Expires == nil || o.Expires.After(now)) {
			e := Effective{Awake: o.State == StateAwake, Source: SourceOverride}
			switch {
			case e.Awake:
				e.Reason = fmt.Sprintf("awake until %s, woken by %s", stamp(*o.Expires), o.By)
			case o.Expires != nil:
				e.Reason = fmt.Sprintf("asleep until %s, put to sleep by %s", stamp(*o.Expires), o.By)
			default:
				e.Reason = "asleep, put to sleep by " + o.By
			}
			return e
		}
	}
	if def == config.Awake {
		return Effective{Awake: true, Source: SourceDefault, Reason: "awake by default"}
	}
	return Effective{Awake: false, Source: SourceDefault, Reason: "asleep by default"}
}

// activeLeases returns the owners of unexpired leases, sorted.
func activeLeases(g *Group, now time.Time) []string {
	var owners []string
	for _, owner := range sortedKeys(g.Leases) {
		if g.Leases[owner].Expires.After(now) {
			owners = append(owners, owner)
		}
	}
	return owners
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
