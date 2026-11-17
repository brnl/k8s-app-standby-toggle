package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
)

// Default is the state a group is in when nothing deviates from it.
type Default string

// The defaults a group can have. "on" and "off" are deliberately not used:
// Helm parses them as booleans when they are not quoted.
const (
	AlwaysOn Default = "always-on"
	Awake    Default = "awake"
	Asleep   Default = "asleep"
)

// Group is a named set of Argo CD Applications that sleep and wake together.
type Group struct {
	Name    string
	Default Default
	Apps    []string
}

var (
	groupNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// Argo CD Application names are Kubernetes object names (DNS-1123
	// subdomains).
	appNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// ParseGroups parses the groups configuration: a JSON object that maps a
// group name to its default and Applications, as rendered by the chart from
// the `groups:` values. Every problem is reported, not only the first.
func ParseGroups(data []byte) ([]Group, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("groups: invalid JSON: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("groups: no groups configured")
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	var (
		errs   []error
		groups []Group
		owner  = map[string]string{} // app → group
	)
	for _, name := range names {
		g, err := parseGroup(name, raw[name])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, app := range g.Apps {
			if other, ok := owner[app]; ok {
				errs = append(errs, fmt.Errorf("groups: Application %q is listed in both group %q and group %q", app, other, name))
				continue
			}
			owner[app] = name
		}
		groups = append(groups, g)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return groups, nil
}

func parseGroup(name string, data json.RawMessage) (Group, error) {
	var errs []error
	if !groupNameRE.MatchString(name) {
		errs = append(errs, fmt.Errorf("groups: invalid group name %q (use a-z, 0-9 and -, at most 63 characters)", name))
	}
	var spec struct {
		Default any      `json:"default"`
		Apps    []string `json:"apps"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return Group{}, fmt.Errorf("groups: group %q: %w", name, err)
	}
	g := Group{Name: name, Apps: spec.Apps}
	switch d := spec.Default.(type) {
	case string:
		switch Default(d) {
		case AlwaysOn, Awake, Asleep:
			g.Default = Default(d)
		default:
			errs = append(errs, fmt.Errorf("groups: group %q: unknown default %q (use always-on, awake or asleep)", name, d))
		}
	case nil:
		errs = append(errs, fmt.Errorf("groups: group %q: default is required (always-on, awake or asleep)", name))
	case bool:
		errs = append(errs, fmt.Errorf("groups: group %q: default is the boolean %v; YAML reads unquoted on/off/yes/no as booleans, use always-on, awake or asleep", name, d))
	default:
		errs = append(errs, fmt.Errorf("groups: group %q: default must be a string (always-on, awake or asleep)", name))
	}
	if len(g.Apps) == 0 {
		errs = append(errs, fmt.Errorf("groups: group %q is empty: list at least one Application in apps", name))
	}
	seen := map[string]bool{}
	for _, app := range g.Apps {
		switch {
		case !appNameRE.MatchString(app):
			errs = append(errs, fmt.Errorf("groups: group %q: invalid Application name %q", name, app))
		case seen[app]:
			errs = append(errs, fmt.Errorf("groups: group %q lists Application %q twice", name, app))
		}
		seen[app] = true
	}
	if len(errs) > 0 {
		return Group{}, errors.Join(errs...)
	}
	g.Apps = slices.Clone(g.Apps)
	return g, nil
}
