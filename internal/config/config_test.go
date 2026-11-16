package config

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

const groupsJSON = `{
  "identity": {"default": "always-on", "apps": ["lldap", "authelia"]},
  "vikunja": {"default": "awake", "apps": ["vikunja", "vikunja-infra"]},
  "nextcloud": {"default": "asleep", "apps": ["nextcloud"]}
}`

func TestParseGroups(t *testing.T) {
	groups, err := ParseGroups([]byte(groupsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3 || groups[0].Name != "identity" || groups[1].Name != "nextcloud" || groups[2].Default != Awake {
		t.Fatalf("groups = %+v", groups)
	}
}

func TestParseGroupsErrors(t *testing.T) {
	tests := map[string]struct{ json, want string }{
		"app in two groups": {`{"a":{"default":"awake","apps":["x"]},"b":{"default":"asleep","apps":["x"]}}`, `"x" is listed in both group "a" and group "b"`},
		"unknown default":   {`{"a":{"default":"on","apps":["x"]}}`, `unknown default "on"`},
		"boolean default":   {`{"a":{"default":true,"apps":["x"]}}`, `unquoted on/off`},
		"missing default":   {`{"a":{"apps":["x"]}}`, `default is required`},
		"empty group":       {`{"a":{"default":"awake","apps":[]}}`, `group "a" is empty`},
		"no apps key":       {`{"a":{"default":"awake"}}`, `group "a" is empty`},
		"no groups":         {`{}`, `no groups configured`},
		"unknown key":       {`{"a":{"default":"awake","apps":["x"],"extra":1}}`, `unknown field`},
		"bad group name":    {`{"A_b":{"default":"awake","apps":["x"]}}`, `invalid group name`},
		"bad app name":      {`{"a":{"default":"awake","apps":["X y"]}}`, `invalid Application name`},
		"duplicate app":     {`{"a":{"default":"awake","apps":["x","x"]}}`, `lists Application "x" twice`},
		"not JSON":          {`groups:`, `invalid JSON`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseGroups([]byte(tt.json))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func load(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	files := map[string]string{DefaultGroupsFile: groupsJSON}
	return Load(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}, func(path string) ([]byte, error) {
		if v, ok := files[path]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	})
}

func validEnv() map[string]string {
	return map[string]string{
		"STANDBY_NAMESPACE":           "standby",
		"STANDBY_OIDC_ISSUER_URL":     "https://auth.example.com",
		"STANDBY_OIDC_REDIRECT_URL":   "https://standby.example.com/auth/callback",
		"STANDBY_OIDC_ALLOWED_GROUPS": "admins, devs",
		"STANDBY_MACHINE_SUBJECTS":    "system:serviceaccount:ci-runner:ci-runner",
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(t, validEnv())
	if err != nil {
		t.Fatal(err)
	}
	if c.ReconcileInterval != 30*time.Second || c.HoldDefault != 24*time.Hour || c.HoldMax != 24*time.Hour ||
		len(c.WakeDurations) != 3 || c.WakeDurations[2] != 24*time.Hour ||
		c.AnnotationPrefix != "k8s-app-standby-toggle.cloud64.net/" || c.ArgoCDNamespace != "argocd" ||
		c.Machine.Audience != "k8s-app-standby-toggle" || len(c.OIDC.AllowedGroups) != 2 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := map[string]struct {
		set  map[string]string
		want string
	}{
		"no allowed groups":          {map[string]string{"STANDBY_OIDC_ALLOWED_GROUPS": ""}, "STANDBY_OIDC_ALLOW_ANY_USER"},
		"auth disabled on all ifs":   {map[string]string{"STANDBY_AUTH_DISABLED": "true"}, "only allowed when STANDBY_LISTEN_ADDR is bound to localhost"},
		"no issuer":                  {map[string]string{"STANDBY_OIDC_ISSUER_URL": ""}, "STANDBY_OIDC_ISSUER_URL is required"},
		"http issuer":                {map[string]string{"STANDBY_OIDC_ISSUER_URL": "http://auth.example.com"}, "must use https"},
		"wrong callback path":        {map[string]string{"STANDBY_OIDC_REDIRECT_URL": "https://x.example.com/cb"}, "/auth/callback"},
		"bad subject":                {map[string]string{"STANDBY_MACHINE_SUBJECTS": "ci-runner"}, "is not a ServiceAccount"},
		"bad duration":               {map[string]string{"STANDBY_WAKE_DURATIONS": "1h,soon"}, `invalid duration "soon"`},
		"hold max below default":     {map[string]string{"STANDBY_HOLD_MAX": "1h"}, "STANDBY_HOLD_MAX"},
		"bad prefix":                 {map[string]string{"STANDBY_ANNOTATION_PREFIX": "example.com"}, "STANDBY_ANNOTATION_PREFIX"},
		"bad groups file":            {map[string]string{"STANDBY_GROUPS_FILE": "/nope"}, "STANDBY_GROUPS_FILE"},
		"no namespace":               {map[string]string{"STANDBY_NAMESPACE": ""}, "STANDBY_NAMESPACE"},
		"interval too short":         {map[string]string{"STANDBY_RECONCILE_INTERVAL": "1s"}, "STANDBY_RECONCILE_INTERVAL"},
		"conflict threshold too low": {map[string]string{"STANDBY_CONFLICT_THRESHOLD": "1"}, "STANDBY_CONFLICT_THRESHOLD"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			for k, v := range tt.set {
				env[k] = v
			}
			_, err := load(t, env)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestAuthDisabledOnLocalhost(t *testing.T) {
	env := map[string]string{"STANDBY_NAMESPACE": "standby", "STANDBY_AUTH_DISABLED": "true", "STANDBY_LISTEN_ADDR": "127.0.0.1:8080"}
	if _, err := load(t, env); err != nil {
		t.Fatal(err)
	}
	env["STANDBY_LISTEN_ADDR"] = "0.0.0.0:8080"
	if _, err := load(t, env); err == nil {
		t.Fatal("auth disabled on 0.0.0.0 accepted")
	}
}

func TestAllowAnyUser(t *testing.T) {
	env := validEnv()
	env["STANDBY_OIDC_ALLOWED_GROUPS"] = ""
	env["STANDBY_OIDC_ALLOW_ANY_USER"] = "true"
	if _, err := load(t, env); err != nil {
		t.Fatal(err)
	}
}

func TestIsLoopbackListenAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "[::1]:8080": true, "localhost:8080": true,
		":8080": false, "0.0.0.0:8080": false, "10.0.0.1:8080": false, "bogus": false,
	} {
		if got := IsLoopbackListenAddr(addr); got != want {
			t.Errorf("IsLoopbackListenAddr(%q) = %v", addr, got)
		}
	}
}

func TestReadSessionKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadSessionKey(dir + "/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}
