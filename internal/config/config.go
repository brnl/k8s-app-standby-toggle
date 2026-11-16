// Package config loads the service configuration from environment variables
// and the groups file.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Defaults for settings that have one.
const (
	DefaultGroupsFile        = "/etc/k8s-app-standby-toggle/groups/groups.json"
	DefaultAnnotationPrefix  = "k8s-app-standby-toggle.cloud64.net/"
	DefaultStateConfigMap    = "k8s-app-standby-toggle-state"
	DefaultMachineAudience   = "k8s-app-standby-toggle"
	DefaultOIDCClientID      = "k8s-app-standby-toggle"
	serviceAccountNamespace  = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	minimumSessionKeyLength  = 32
	maximumReconcileInterval = time.Hour
)

// DefaultWakeDurations are the durations a person can wake a group for.
var DefaultWakeDurations = []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour}

// Config holds the runtime configuration.
type Config struct {
	Groups     []Group
	GroupsFile string

	// Namespace is the service's own namespace (state ConfigMap, never
	// touched).
	Namespace       string
	ArgoCDNamespace string

	ReconcileInterval time.Duration
	// WakeDurations are the durations an "awake" override may have.
	WakeDurations []time.Duration
	HoldDefault   time.Duration
	HoldMax       time.Duration
	LeaseMax      time.Duration
	// DegradedAfter is how long a group may be waking before it is reported
	// as degraded.
	DegradedAfter time.Duration
	// DatabaseWait bounds the wait for databases to be ready before the rest
	// of a group is woken, and for the rest to be gone before databases are
	// hibernated.
	DatabaseWait time.Duration
	// ConflictThreshold undone sleeps within ConflictWindow mark a group as
	// in conflict.
	ConflictThreshold int
	ConflictWindow    time.Duration

	AnnotationPrefix string
	StateConfigMap   string

	ListenAddr string
	// KubeAPIURL overrides in-cluster access (e.g. http://127.0.0.1:8001 with
	// `kubectl proxy`); empty means in-cluster.
	KubeAPIURL string

	AuthDisabled bool
	OIDC         OIDC
	Machine      Machine

	LogLevel  string
	LogFormat string
}

// OIDC configures login for people.
type OIDC struct {
	IssuerURL     string
	ClientID      string
	RedirectURL   string
	CAFile        string
	AllowedGroups []string
	// AllowAnyUser lets every authenticated user in when AllowedGroups is
	// empty.
	AllowAnyUser   bool
	SessionKeyFile string
	SessionTTL     time.Duration
}

// Machine configures ServiceAccount token authentication.
type Machine struct {
	Audience string
	Subjects []string
}

// Load reads the configuration with lookup (normally os.LookupEnv) and
// readFile (normally os.ReadFile).
func Load(lookup func(string) (string, bool), readFile func(string) ([]byte, error)) (*Config, error) {
	e := env{lookup: lookup}
	c := &Config{
		GroupsFile:        e.str("STANDBY_GROUPS_FILE", DefaultGroupsFile),
		Namespace:         e.str("STANDBY_NAMESPACE", ""),
		ArgoCDNamespace:   e.str("STANDBY_ARGOCD_NAMESPACE", "argocd"),
		ReconcileInterval: e.duration("STANDBY_RECONCILE_INTERVAL", 30*time.Second),
		WakeDurations:     e.durations("STANDBY_WAKE_DURATIONS", DefaultWakeDurations),
		HoldDefault:       e.duration("STANDBY_HOLD_DEFAULT", 24*time.Hour),
		HoldMax:           e.duration("STANDBY_HOLD_MAX", 24*time.Hour),
		LeaseMax:          e.duration("STANDBY_LEASE_MAX", 4*time.Hour),
		DegradedAfter:     e.duration("STANDBY_DEGRADED_AFTER", 10*time.Minute),
		DatabaseWait:      e.duration("STANDBY_DATABASE_WAIT", 5*time.Minute),
		ConflictThreshold: e.int("STANDBY_CONFLICT_THRESHOLD", 3),
		ConflictWindow:    e.duration("STANDBY_CONFLICT_WINDOW", 10*time.Minute),
		AnnotationPrefix:  e.str("STANDBY_ANNOTATION_PREFIX", DefaultAnnotationPrefix),
		StateConfigMap:    e.str("STANDBY_STATE_CONFIGMAP", DefaultStateConfigMap),
		ListenAddr:        e.str("STANDBY_LISTEN_ADDR", ":8080"),
		KubeAPIURL:        e.str("STANDBY_KUBE_API_URL", ""),
		AuthDisabled:      e.bool("STANDBY_AUTH_DISABLED", false),
		OIDC: OIDC{
			IssuerURL:      e.str("STANDBY_OIDC_ISSUER_URL", ""),
			ClientID:       e.str("STANDBY_OIDC_CLIENT_ID", DefaultOIDCClientID),
			RedirectURL:    e.str("STANDBY_OIDC_REDIRECT_URL", ""),
			CAFile:         e.str("STANDBY_OIDC_CA_FILE", ""),
			AllowedGroups:  e.list("STANDBY_OIDC_ALLOWED_GROUPS"),
			AllowAnyUser:   e.bool("STANDBY_OIDC_ALLOW_ANY_USER", false),
			SessionKeyFile: e.str("STANDBY_SESSION_KEY_FILE", ""),
			SessionTTL:     e.duration("STANDBY_SESSION_TTL", 12*time.Hour),
		},
		Machine: Machine{
			Audience: e.str("STANDBY_MACHINE_AUDIENCE", DefaultMachineAudience),
			Subjects: e.list("STANDBY_MACHINE_SUBJECTS"),
		},
		LogLevel:  strings.ToLower(e.str("STANDBY_LOG_LEVEL", "info")),
		LogFormat: strings.ToLower(e.str("STANDBY_LOG_FORMAT", "json")),
	}
	errs := e.errs

	if c.Namespace == "" {
		if b, err := readFile(serviceAccountNamespace); err == nil {
			c.Namespace = strings.TrimSpace(string(b))
		}
	}

	if data, err := readFile(c.GroupsFile); err != nil {
		errs = append(errs, fmt.Errorf("STANDBY_GROUPS_FILE: %w", err))
	} else if c.Groups, err = ParseGroups(data); err != nil {
		errs = append(errs, err)
	}

	errs = append(errs, c.validate()...)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

var (
	dnsLabelRE     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	dnsSubdomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	subjectRE      = regexp.MustCompile(`^system:serviceaccount:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?:[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

func (c *Config) validate() []error {
	var errs []error
	if c.Namespace == "" {
		errs = append(errs, errors.New("STANDBY_NAMESPACE: not set and not running in a pod"))
	} else if !dnsLabelRE.MatchString(c.Namespace) {
		errs = append(errs, fmt.Errorf("STANDBY_NAMESPACE: invalid namespace %q", c.Namespace))
	}
	if !dnsLabelRE.MatchString(c.ArgoCDNamespace) {
		errs = append(errs, fmt.Errorf("STANDBY_ARGOCD_NAMESPACE: invalid namespace %q", c.ArgoCDNamespace))
	}
	if c.ReconcileInterval < 5*time.Second || c.ReconcileInterval > maximumReconcileInterval {
		errs = append(errs, errors.New("STANDBY_RECONCILE_INTERVAL must be between 5s and 1h"))
	}
	if len(c.WakeDurations) == 0 {
		errs = append(errs, errors.New("STANDBY_WAKE_DURATIONS: at least one duration is required"))
	}
	for _, d := range c.WakeDurations {
		if d < time.Minute {
			errs = append(errs, fmt.Errorf("STANDBY_WAKE_DURATIONS: %s is shorter than 1m", d))
		}
	}
	slices.Sort(c.WakeDurations)
	c.WakeDurations = slices.Compact(c.WakeDurations)
	if c.HoldDefault < time.Minute {
		errs = append(errs, errors.New("STANDBY_HOLD_DEFAULT must be at least 1m"))
	}
	if c.HoldMax < c.HoldDefault {
		errs = append(errs, errors.New("STANDBY_HOLD_MAX must not be shorter than STANDBY_HOLD_DEFAULT"))
	}
	if c.LeaseMax < time.Minute {
		errs = append(errs, errors.New("STANDBY_LEASE_MAX must be at least 1m"))
	}
	if c.DegradedAfter < time.Minute {
		errs = append(errs, errors.New("STANDBY_DEGRADED_AFTER must be at least 1m"))
	}
	if c.DatabaseWait < 10*time.Second {
		errs = append(errs, errors.New("STANDBY_DATABASE_WAIT must be at least 10s"))
	}
	if c.ConflictThreshold < 2 {
		errs = append(errs, errors.New("STANDBY_CONFLICT_THRESHOLD must be at least 2"))
	}
	if c.ConflictWindow < time.Minute {
		errs = append(errs, errors.New("STANDBY_CONFLICT_WINDOW must be at least 1m"))
	}
	if prefix, ok := strings.CutSuffix(c.AnnotationPrefix, "/"); !ok || len(prefix) > 253 || !dnsSubdomainRE.MatchString(prefix) {
		errs = append(errs, fmt.Errorf("STANDBY_ANNOTATION_PREFIX: %q must be a DNS subdomain followed by \"/\"", c.AnnotationPrefix))
	}
	if len(c.StateConfigMap) > 253 || !dnsSubdomainRE.MatchString(c.StateConfigMap) {
		errs = append(errs, fmt.Errorf("STANDBY_STATE_CONFIGMAP: invalid name %q", c.StateConfigMap))
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("STANDBY_LISTEN_ADDR: %w", err))
	}
	if c.KubeAPIURL != "" {
		if u, err := url.Parse(c.KubeAPIURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("STANDBY_KUBE_API_URL: invalid URL %q", c.KubeAPIURL))
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("STANDBY_LOG_LEVEL: invalid level %q", c.LogLevel))
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("STANDBY_LOG_FORMAT: invalid format %q", c.LogFormat))
	}
	if c.AuthDisabled {
		if !IsLoopbackListenAddr(c.ListenAddr) {
			errs = append(errs, fmt.Errorf("STANDBY_AUTH_DISABLED is only allowed when STANDBY_LISTEN_ADDR is bound to localhost (e.g. 127.0.0.1:8080), not %q", c.ListenAddr))
		}
	} else {
		errs = append(errs, c.validateAuth()...)
	}
	return errs
}

func (c *Config) validateAuth() []error {
	var errs []error
	o := c.OIDC
	if o.IssuerURL == "" {
		errs = append(errs, errors.New("STANDBY_OIDC_ISSUER_URL is required (or set STANDBY_AUTH_DISABLED for local development)"))
	} else if err := checkURL(o.IssuerURL); err != nil {
		errs = append(errs, fmt.Errorf("STANDBY_OIDC_ISSUER_URL: %w", err))
	}
	if o.ClientID == "" {
		errs = append(errs, errors.New("STANDBY_OIDC_CLIENT_ID is required"))
	}
	if o.RedirectURL == "" {
		errs = append(errs, errors.New("STANDBY_OIDC_REDIRECT_URL is required, e.g. https://standby.example.com/auth/callback"))
	} else if err := checkURL(o.RedirectURL); err != nil {
		errs = append(errs, fmt.Errorf("STANDBY_OIDC_REDIRECT_URL: %w", err))
	} else if u, _ := url.Parse(o.RedirectURL); u.Path != "/auth/callback" {
		errs = append(errs, fmt.Errorf("STANDBY_OIDC_REDIRECT_URL: the path must be /auth/callback, not %q", u.Path))
	}
	if len(o.AllowedGroups) == 0 && !o.AllowAnyUser {
		errs = append(errs, errors.New("STANDBY_OIDC_ALLOWED_GROUPS is empty: list the groups that may use the service, or set STANDBY_OIDC_ALLOW_ANY_USER=true to allow every authenticated user"))
	}
	if o.SessionTTL < 5*time.Minute || o.SessionTTL > 7*24*time.Hour {
		errs = append(errs, errors.New("STANDBY_SESSION_TTL must be between 5m and 168h"))
	}
	if c.Machine.Audience == "" {
		errs = append(errs, errors.New("STANDBY_MACHINE_AUDIENCE must not be empty"))
	}
	for _, s := range c.Machine.Subjects {
		if !subjectRE.MatchString(s) {
			errs = append(errs, fmt.Errorf("STANDBY_MACHINE_SUBJECTS: %q is not a ServiceAccount (system:serviceaccount:<namespace>:<name>)", s))
		}
	}
	return errs
}

// checkURL requires https, except for loopback hosts (local development).
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid URL %q", raw)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
	default:
		return fmt.Errorf("%q must use https", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must not have a query or fragment", raw)
	}
	return nil
}

// IsLoopbackListenAddr reports whether addr only listens on a loopback
// interface. An empty host (":8080") listens on every interface.
func IsLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ReadSessionKey reads the session signing key from path. It must hold at
// least 32 bytes.
func ReadSessionKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("STANDBY_SESSION_KEY_FILE: %w", err)
	}
	key := []byte(strings.TrimSpace(string(b)))
	if len(key) < minimumSessionKeyLength {
		return nil, fmt.Errorf("STANDBY_SESSION_KEY_FILE: the key must be at least %d bytes", minimumSessionKeyLength)
	}
	return key, nil
}

type env struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (e *env) str(key, def string) string {
	if v, ok := e.lookup(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (e *env) list(key string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(e.str(key, ""), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func (e *env) bool(key string, def bool) bool {
	v := e.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: invalid boolean %q", key, v))
		return def
	}
	return b
}

func (e *env) int(key string, def int) int {
	v := e.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: invalid number %q", key, v))
		return def
	}
	return n
}

func (e *env) duration(key string, def time.Duration) time.Duration {
	v := e.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: invalid duration %q (use e.g. 30s, 10m, 4h)", key, v))
		return def
	}
	return d
}

func (e *env) durations(key string, def []time.Duration) []time.Duration {
	items := e.list(key)
	if len(items) == 0 {
		return slices.Clone(def)
	}
	out := make([]time.Duration, 0, len(items))
	for _, v := range items {
		d, err := time.ParseDuration(v)
		if err != nil {
			e.errs = append(e.errs, fmt.Errorf("%s: invalid duration %q (use e.g. 1h,4h,24h)", key, v))
			continue
		}
		out = append(out, d)
	}
	return out
}
