// Package web serves the HTTP API, the web UI, metrics and health endpoints
// on one listener.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/brnl/k8s-app-standby-toggle/internal/auth"
	"github.com/brnl/k8s-app-standby-toggle/internal/controller"
)

//go:embed static
var staticFiles embed.FS

// The page loads one script and one stylesheet from /static and nothing
// else.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// Options configures the handler.
type Options struct {
	Controller *controller.Controller
	Auth       *auth.Authenticator
	// WakeDurations are the allowed durations of an awake override.
	WakeDurations []time.Duration
	HoldDefault   time.Duration
	HoldMax       time.Duration
	LeaseMax      time.Duration
	Version       string
	Log           *slog.Logger
}

type server struct {
	Options
}

// Handler returns the HTTP handler.
func Handler(o Options) http.Handler {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	s := &server{o}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { respondHealth(w, s.Controller.Live()) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { respondHealth(w, s.Controller.Ready()) })
	mux.HandleFunc("GET /metrics", s.metrics)

	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /ui/groups", s.fragment)
	static, _ := fs.Sub(staticFiles, "static")
	fileServer := http.StripPrefix("/static/", http.FileServerFS(static))
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fileServer.ServeHTTP(w, r)
	})

	mux.HandleFunc("GET /auth/login", s.Auth.Login)
	mux.HandleFunc("GET /auth/callback", s.Auth.Callback)
	mux.HandleFunc("POST /auth/logout", s.Auth.Logout)
	mux.HandleFunc("GET /auth/signed-out", s.Auth.SignedOut)

	mux.HandleFunc("GET /api/v1/whoami", s.api(s.whoami))
	mux.HandleFunc("GET /api/v1/groups", s.api(s.listGroups))
	mux.HandleFunc("GET /api/v1/groups/{group}", s.api(s.getGroup))
	mux.HandleFunc("PUT /api/v1/groups/{group}/override", s.api(s.putOverride))
	mux.HandleFunc("DELETE /api/v1/groups/{group}/override", s.api(s.deleteOverride))
	mux.HandleFunc("PUT /api/v1/groups/{group}/leases/{owner}", s.api(s.putLease))
	mux.HandleFunc("DELETE /api/v1/groups/{group}/leases/{owner}", s.api(s.deleteLease))
	mux.HandleFunc("PUT /api/v1/groups/{group}/hold", s.api(s.putHold))
	mux.HandleFunc("DELETE /api/v1/groups/{group}/hold", s.api(s.deleteHold))
	mux.HandleFunc("DELETE /api/v1/groups/{group}/conflict", s.api(s.deleteConflict))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "no such API endpoint")
	})

	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func respondHealth(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(err.Error() + "\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// Serve runs the HTTP server until ctx is cancelled.
func Serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
