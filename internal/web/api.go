package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/brnl/k8s-app-standby-toggle/internal/auth"
	"github.com/brnl/k8s-app-standby-toggle/internal/config"
	"github.com/brnl/k8s-app-standby-toggle/internal/state"
)

const (
	maxBodyBytes  = 16 << 10
	maxReasonLen  = 500
	leaseOwnerMax = 128
)

var leaseOwnerRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// api authenticates a request before calling h.
func (s *server) api(h func(http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, fail := s.Auth.Authenticate(r)
		if fail != nil {
			writeError(w, fail.Status, fail.Message)
			return
		}
		h(w, r, p)
	}
}

func (s *server) whoami(w http.ResponseWriter, _ *http.Request, p auth.Principal) {
	writeJSON(w, http.StatusOK, map[string]any{"name": p.Name, "machine": p.Machine})
}

func (s *server) listGroups(w http.ResponseWriter, _ *http.Request, _ auth.Principal) {
	writeJSON(w, http.StatusOK, s.Controller.Views())
}

func (s *server) getGroup(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	v, ok := s.Controller.View(r.PathValue("group"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown group %q", r.PathValue("group")))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// mutable checks that the group exists and may be changed, and answers the
// request if not.
func (s *server) mutable(w http.ResponseWriter, r *http.Request) (config.Group, bool) {
	name := r.PathValue("group")
	g, ok := s.Controller.Group(name)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown group %q", name))
		return g, false
	}
	if g.Default == config.AlwaysOn {
		writeError(w, http.StatusConflict, fmt.Sprintf("group %q is always-on and cannot be changed", name))
		return g, false
	}
	return g, true
}

// peopleOnly refuses machines.
func peopleOnly(w http.ResponseWriter, p auth.Principal, what string) bool {
	if p.Machine {
		writeError(w, http.StatusForbidden, "machines may not "+what)
		return false
	}
	return true
}

// mutate applies fn to the group and answers with the updated group. It
// reports whether the change was stored.
func (s *server) mutate(w http.ResponseWriter, r *http.Request, name string, fn func(g *state.Group, now time.Time) error) bool {
	v, err := s.Controller.Mutate(r.Context(), name, fn)
	if err != nil {
		s.Log.Error("storing a change failed", "group", name, "error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "cannot store the change right now: "+err.Error())
		return false
	}
	writeJSON(w, http.StatusOK, v)
	return true
}

func (s *server) putOverride(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !peopleOnly(w, p, "set overrides; use a lease") {
		return
	}
	g, ok := s.mutable(w, r)
	if !ok {
		return
	}
	var body struct {
		State string `json:"state"`
		TTL   string `json:"ttl"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var ttl time.Duration
	switch body.State {
	case state.StateAwake:
		d, err := parseTTL(body.TTL, true)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !slices.Contains(s.WakeDurations, d) {
			writeError(w, http.StatusBadRequest, "ttl must be one of "+durationList(s.WakeDurations))
			return
		}
		ttl = d
	case state.StateAsleep:
		d, err := parseTTL(body.TTL, false)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		ttl = d
	default:
		writeError(w, http.StatusBadRequest, `state must be "awake" or "asleep"`)
		return
	}
	if !s.mutate(w, r, g.Name, func(gs *state.Group, now time.Time) error {
		o := &state.Override{State: body.State, By: p.Name, Set: now}
		if ttl > 0 {
			exp := now.Add(ttl)
			o.Expires = &exp
		}
		gs.Override = o
		return nil
	}) {
		return
	}
	s.Log.Info("override set", "group", g.Name, "state", body.State, "ttl", ttlString(ttl), "by", p.Name)
}

func (s *server) deleteOverride(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !peopleOnly(w, p, "delete overrides") {
		return
	}
	g, ok := s.mutable(w, r)
	if !ok {
		return
	}
	existed := false
	if !s.mutate(w, r, g.Name, func(gs *state.Group, _ time.Time) error {
		existed = gs.Override != nil
		gs.Override = nil
		return nil
	}) {
		return
	}
	if existed {
		s.Log.Info("override removed: back to the default", "group", g.Name, "by", p.Name)
	}
}

func (s *server) putLease(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	owner := r.PathValue("owner")
	if len(owner) > leaseOwnerMax || !leaseOwnerRE.MatchString(owner) {
		writeError(w, http.StatusBadRequest, "invalid lease owner: use letters, digits, '.', '_' and '-' (at most 128)")
		return
	}
	g, ok := s.mutable(w, r)
	if !ok {
		return
	}
	var body struct {
		TTL string `json:"ttl"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ttl, err := parseTTL(body.TTL, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ttl > s.LeaseMax {
		writeError(w, http.StatusBadRequest, "ttl must not exceed "+s.LeaseMax.String())
		return
	}
	renewed := false
	if !s.mutate(w, r, g.Name, func(gs *state.Group, now time.Time) error {
		if gs.Leases == nil {
			gs.Leases = map[string]state.Lease{}
		}
		l := state.Lease{By: p.Name, Expires: now.Add(ttl), Created: now}
		if old, ok := gs.Leases[owner]; ok && old.Expires.After(now) {
			l.Created, renewed = old.Created, true
		}
		gs.Leases[owner] = l
		return nil
	}) {
		return
	}
	msg := "lease created"
	if renewed {
		msg = "lease renewed"
	}
	s.Log.Info(msg, "group", g.Name, "owner", owner, "ttl", ttl.String(), "by", p.Name)
}

func (s *server) deleteLease(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	owner := r.PathValue("owner")
	g, ok := s.mutable(w, r)
	if !ok {
		return
	}
	existed := false
	if !s.mutate(w, r, g.Name, func(gs *state.Group, _ time.Time) error {
		_, existed = gs.Leases[owner]
		delete(gs.Leases, owner)
		return nil
	}) {
		return
	}
	if existed {
		s.Log.Info("lease released", "group", g.Name, "owner", owner, "by", p.Name)
	}
}

func (s *server) putHold(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	g, ok := s.mutable(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
		TTL    string `json:"ttl"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	reason := strings.TrimSpace(body.Reason)
	switch {
	case reason == "":
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	case len(reason) > maxReasonLen:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("reason must not exceed %d bytes", maxReasonLen))
		return
	case strings.IndexFunc(reason, unicode.IsControl) >= 0:
		writeError(w, http.StatusBadRequest, "reason must be a single line of text")
		return
	}
	ttl, err := parseTTL(body.TTL, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ttl == 0 {
		ttl = s.HoldDefault
	}
	if ttl > s.HoldMax {
		writeError(w, http.StatusBadRequest, "ttl must not exceed "+s.HoldMax.String())
		return
	}
	if !s.mutate(w, r, g.Name, func(gs *state.Group, now time.Time) error {
		h := &state.Hold{Reason: reason, By: p.Name, Expires: now.Add(ttl), Set: now}
		// A machine may not lift a hold, so it may not shorten one either.
		if old := gs.Hold; p.Machine && old != nil && old.Expires.After(h.Expires) {
			h.Expires = old.Expires
		}
		gs.Hold = h
		return nil
	}) {
		return
	}
	s.Log.Info("hold set", "group", g.Name, "reason", reason, "ttl", ttl.String(), "by", p.Name)
}

func (s *server) deleteHold(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !peopleOnly(w, p, "lift a hold") {
		return
	}
	g, ok := s.mutable(w, r)
	if !ok {
		return
	}
	existed := false
	if !s.mutate(w, r, g.Name, func(gs *state.Group, _ time.Time) error {
		existed = gs.Hold != nil
		gs.Hold = nil
		return nil
	}) {
		return
	}
	if existed {
		s.Log.Info("hold lifted", "group", g.Name, "by", p.Name)
	}
}

func (s *server) deleteConflict(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !peopleOnly(w, p, "clear a conflict") {
		return
	}
	name := r.PathValue("group")
	if _, ok := s.Controller.Group(name); !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown group %q", name))
		return
	}
	existed := false
	if !s.mutate(w, r, name, func(gs *state.Group, _ time.Time) error {
		existed = gs.Conflict != nil
		gs.Conflict = nil
		return nil
	}) {
		return
	}
	if existed {
		s.Log.Info("conflict cleared: acting on the group again", "group", name, "by", p.Name)
	}
}

// decode reads a JSON object body into v, rejecting unknown fields.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("the request body must be a JSON object")
		}
		return fmt.Errorf("invalid JSON body: %v", err)
	}
	if dec.More() {
		return errors.New("invalid JSON body: unexpected data after the object")
	}
	return nil
}

// parseTTL parses a Go duration string; an empty string is 0 unless
// required.
func parseTTL(v string, required bool) (time.Duration, error) {
	if v == "" {
		if required {
			return 0, errors.New(`ttl is required, e.g. "45m"`)
		}
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid ttl %q: use a duration such as \"45m\" or \"4h\"", v)
	}
	if d <= 0 {
		return 0, errors.New("ttl must be positive")
	}
	return d, nil
}

func durationList(ds []time.Duration) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = shortDuration(d)
	}
	return strings.Join(parts, ", ")
}

func ttlString(d time.Duration) string {
	if d == 0 {
		return "none"
	}
	return d.String()
}

// shortDuration formats whole hours and minutes compactly: "1h", "90m",
// "24h".
func shortDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}
