// Command k8s-app-standby-toggle puts groups of Argo CD Applications to sleep
// (scaled to zero) and wakes them again, on a development cluster.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/brnl/k8s-app-standby-toggle/internal/auth"
	"github.com/brnl/k8s-app-standby-toggle/internal/config"
	"github.com/brnl/k8s-app-standby-toggle/internal/controller"
	"github.com/brnl/k8s-app-standby-toggle/internal/kube"
	"github.com/brnl/k8s-app-standby-toggle/internal/standby"
	"github.com/brnl/k8s-app-standby-toggle/internal/state"
	"github.com/brnl/k8s-app-standby-toggle/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "version", "--version", "-v":
			fmt.Println(version)
			return
		default:
			fmt.Fprintf(os.Stderr, "usage: %s [healthcheck|version]\nAll settings are environment variables; see the README.\n", os.Args[0])
			os.Exit(2)
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.LookupEnv, os.ReadFile)
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	userAgent := "k8s-app-standby-toggle/" + version
	var client *kube.Client
	if cfg.KubeAPIURL != "" {
		client = kube.New(cfg.KubeAPIURL, nil, nil, userAgent)
	} else if client, err = kube.NewInCluster(userAgent); err != nil {
		return err
	}

	authn := &auth.Authenticator{Disabled: cfg.AuthDisabled, SessionTTL: cfg.OIDC.SessionTTL, Log: log}
	if !cfg.AuthDisabled {
		var key []byte
		if cfg.OIDC.SessionKeyFile != "" {
			if key, err = config.ReadSessionKey(cfg.OIDC.SessionKeyFile); err != nil {
				return err
			}
		}
		issuerClient, err := auth.IssuerHTTPClient(cfg.OIDC.CAFile)
		if err != nil {
			return err
		}
		authn.Signer = auth.NewSigner(key, nil)
		authn.OIDC = auth.NewOIDC(auth.OIDCConfig{
			IssuerURL:     cfg.OIDC.IssuerURL,
			ClientID:      cfg.OIDC.ClientID,
			RedirectURL:   cfg.OIDC.RedirectURL,
			HTTPClient:    issuerClient,
			AllowedGroups: cfg.OIDC.AllowedGroups,
			AllowAnyUser:  cfg.OIDC.AllowAnyUser,
		})
		authn.Reviewer = auth.NewTokenReviewer(client, cfg.Machine.Audience, cfg.Machine.Subjects, nil)
		if key == nil {
			log.Warn("no session key configured: sessions end when the service restarts")
		}
	} else {
		log.Warn("AUTHENTICATION IS DISABLED: everyone who can reach the listener has full access", "listen_addr", cfg.ListenAddr)
	}

	ctrl := controller.New(controller.Options{
		Groups:            cfg.Groups,
		ArgoCDNamespace:   cfg.ArgoCDNamespace,
		Namespace:         cfg.Namespace,
		Client:            client,
		Store:             state.NewConfigMapStore(client, cfg.Namespace, cfg.StateConfigMap),
		Actuator:          standby.New(client, cfg.AnnotationPrefix),
		Interval:          cfg.ReconcileInterval,
		DegradedAfter:     cfg.DegradedAfter,
		DatabaseWait:      cfg.DatabaseWait,
		ConflictThreshold: cfg.ConflictThreshold,
		ConflictWindow:    cfg.ConflictWindow,
		Log:               log,
	})

	groups := make([]string, len(cfg.Groups))
	for i, g := range cfg.Groups {
		groups[i] = fmt.Sprintf("%s=%s(%s)", g.Name, g.Default, strings.Join(g.Apps, ","))
	}
	log.Info("starting k8s-app-standby-toggle",
		"version", version,
		"namespace", cfg.Namespace,
		"argocd_namespace", cfg.ArgoCDNamespace,
		"groups", strings.Join(groups, " "),
		"reconcile_interval", cfg.ReconcileInterval.String(),
		"wake_durations", durations(cfg.WakeDurations),
		"hold_default", cfg.HoldDefault.String(),
		"hold_max", cfg.HoldMax.String(),
		"lease_max", cfg.LeaseMax.String(),
		"degraded_after", cfg.DegradedAfter.String(),
		"database_wait", cfg.DatabaseWait.String(),
		"conflict", fmt.Sprintf("%d in %s", cfg.ConflictThreshold, cfg.ConflictWindow),
		"annotation_prefix", cfg.AnnotationPrefix,
		"state_configmap", cfg.StateConfigMap,
		"oidc_issuer", cfg.OIDC.IssuerURL,
		"oidc_client_id", cfg.OIDC.ClientID,
		"oidc_allowed_groups", strings.Join(cfg.OIDC.AllowedGroups, ","),
		"oidc_allow_any_user", cfg.OIDC.AllowAnyUser,
		"machine_audience", cfg.Machine.Audience,
		"machine_subjects", strings.Join(cfg.Machine.Subjects, ","),
	)

	handler := web.Handler(web.Options{
		Controller:    ctrl,
		Auth:          authn,
		WakeDurations: cfg.WakeDurations,
		HoldDefault:   cfg.HoldDefault,
		HoldMax:       cfg.HoldMax,
		LeaseMax:      cfg.LeaseMax,
		Version:       version,
		Log:           log,
	})
	go func() {
		if err := web.Serve(ctx, cfg.ListenAddr, handler); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP server stopped", "error", err.Error())
			stop()
		}
	}()
	log.Info("serving HTTP", "addr", cfg.ListenAddr)

	ctrl.Run(ctx)
	log.Info("shutting down")
	return nil
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

func durations(ds []time.Duration) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = d.String()
	}
	return strings.Join(parts, ",")
}

// healthcheck probes /healthz; it is the container HEALTHCHECK, since the
// image has no shell or curl.
func healthcheck() int {
	addr := strings.TrimSpace(os.Getenv("STANDBY_LISTEN_ADDR"))
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid STANDBY_LISTEN_ADDR:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "health check returned", resp.Status)
		return 1
	}
	return 0
}
