package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/brnl/k8s-app-standby-toggle/internal/kube/kubetest"
)

func TestSessionCookie(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := NewSigner([]byte(strings.Repeat("k", 32)), func() time.Time { return now })
	sess := s.NewSession("bram", time.Hour)
	v := s.EncodeSession(sess)

	got, err := s.DecodeSession(v)
	if err != nil || got.User != "bram" || got.CSRF == "" {
		t.Fatalf("DecodeSession = %+v, %v", got, err)
	}
	// Tampering breaks the signature.
	payload, sig, _ := strings.Cut(v, ".")
	forged, _ := json.Marshal(Session{User: "admin", CSRF: "x", Expires: sess.Expires})
	if _, err := s.DecodeSession(base64.RawURLEncoding.EncodeToString(forged) + "." + sig); err == nil {
		t.Fatal("forged session accepted")
	}
	if _, err := s.DecodeSession(payload); err == nil {
		t.Fatal("unsigned session accepted")
	}
	// Another key does not verify it.
	if _, err := NewSigner([]byte(strings.Repeat("x", 32)), nil).DecodeSession(v); err == nil {
		t.Fatal("session accepted with another key")
	}
	// A login cookie is not a session.
	if _, err := s.DecodeSession(s.encodeLogin(loginState{State: "s", Expires: now.Add(time.Hour).Unix()})); err == nil {
		t.Fatal("login cookie accepted as session")
	}
	// Expired.
	now = now.Add(time.Hour)
	if _, err := s.DecodeSession(v); err == nil {
		t.Fatal("expired session accepted")
	}
}

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/": "/", "/x?y=1": "/x?y=1", "": "/", "https://evil.example": "/", "//evil.example": "/", "/\\evil.example": "/", "/a\r\nSet-Cookie: x": "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTokenReviewer(t *testing.T) {
	api := kubetest.New(t)
	api.TokenReview = func(token string, auds []string) (bool, string, []string) {
		switch token {
		case "ci":
			return true, "system:serviceaccount:ci-runner:ci-runner", auds
		case "other":
			return true, "system:serviceaccount:default:default", auds
		case "wrong-audience":
			return true, "system:serviceaccount:ci-runner:ci-runner", []string{"https://kubernetes.default.svc"}
		}
		return false, "", nil
	}
	r := NewTokenReviewer(api.Client(), "k8s-app-standby-toggle", []string{"system:serviceaccount:ci-runner:ci-runner"}, nil)
	ctx := context.Background()

	if user, err := r.Review(ctx, "ci"); err != nil || user != "system:serviceaccount:ci-runner:ci-runner" {
		t.Fatalf("ci: %q %v", user, err)
	}
	if user, err := r.Review(ctx, "other"); !errors.Is(err, ErrForbiddenMember) || user != "system:serviceaccount:default:default" {
		t.Fatalf("other: %q %v", user, err)
	}
	if _, err := r.Review(ctx, "wrong-audience"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("wrong audience: %v", err)
	}
	if _, err := r.Review(ctx, "garbage"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("garbage: %v", err)
	}
	// The audience is sent with every review.
	for _, req := range api.Requests() {
		if !strings.Contains(req.Body, `"audiences":["k8s-app-standby-toggle"]`) {
			t.Fatalf("review without audience: %s", req.Body)
		}
	}
	// Results are cached.
	n := api.Count(http.MethodPost, "/apis/authentication.k8s.io")
	for range 5 {
		_, _ = r.Review(ctx, "ci")
	}
	if api.Count(http.MethodPost, "/apis/authentication.k8s.io") != n {
		t.Fatal("repeated reviews not cached")
	}
}

// fakeIssuer is a minimal OpenID provider.
type fakeIssuer struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu    sync.Mutex
	codes map[string]codeGrant
	// userinfo answers for the next login.
	user, sub string
	groups    []string
	// idTokenSub overrides the ID token's subject (to test a mismatch).
	idTokenSub string
}

type codeGrant struct{ challenge, nonce string }

func newFakeIssuer(t *testing.T) *fakeIssuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{t: t, key: key, codes: map[string]codeGrant{}, user: "bram", sub: "u-1", groups: []string{"admins"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"userinfo_endpoint":                     f.srv.URL + "/userinfo",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-token" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": f.sub, "preferred_username": f.user, "groups": f.groups})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// authorize plays the browser at the provider: it issues a code for the
// authorization URL the service redirected to.
func (f *fakeIssuer) authorize(authURL string) (code, state string) {
	u, err := url.Parse(authURL)
	if err != nil {
		f.t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		f.t.Fatalf("no PKCE in %s", authURL)
	}
	if q.Get("client_id") != "standby" || q.Get("response_type") != "code" || q.Get("scope") != "openid profile groups" {
		f.t.Fatalf("unexpected authorization request %s", authURL)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	code = "code-" + q.Get("state")[:8]
	f.codes[code] = codeGrant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce")}
	return code, q.Get("state")
}

func (f *fakeIssuer) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	grant, ok := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case !ok:
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != grant.challenge:
		http.Error(w, `{"error":"invalid_grant","error_description":"PKCE"}`, http.StatusBadRequest)
		return
	case r.PostForm.Get("client_id") != "standby" || r.PostForm.Has("client_secret") || r.Header.Get("Authorization") != "":
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	sub := f.sub
	if f.idTokenSub != "" {
		sub = f.idTokenSub
	}
	claims, _ := json.Marshal(map[string]any{
		"iss": f.srv.URL, "sub": sub, "aud": "standby", "nonce": grant.nonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: f.key, KeyID: "k1"}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		f.t.Fatal(err)
	}
	jws, err := signer.Sign(claims)
	if err != nil {
		f.t.Fatal(err)
	}
	idToken, _ := jws.CompactSerialize()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-token", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
}

func newAuthenticator(f *fakeIssuer, allowed []string) *Authenticator {
	return &Authenticator{
		OIDC: NewOIDC(OIDCConfig{
			IssuerURL: f.srv.URL, ClientID: "standby", RedirectURL: "https://standby.example.com/auth/callback",
			HTTPClient: f.srv.Client(), AllowedGroups: allowed,
		}),
		Signer:     NewSigner(nil, nil),
		SessionTTL: time.Hour,
		Log:        slog.New(slog.DiscardHandler),
	}
}

// login runs /auth/login and /auth/callback and returns the callback
// response.
func login(t *testing.T, f *fakeIssuer, a *Authenticator, tamperState bool) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login?return=/groups", nil))
	resp := rec.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	code, state := f.authorize(resp.Header.Get("Location"))
	if tamperState {
		state = "x" + state
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	a.Callback(rec, req)
	return rec.Result()
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestOIDCLogin(t *testing.T) {
	f := newFakeIssuer(t)
	a := newAuthenticator(f, []string{"admins"})
	resp := login(t, f, a, false)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/groups" {
		t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	c := sessionCookie(resp)
	if c == nil || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie %+v", c)
	}
	sess, err := a.Signer.DecodeSession(c.Value)
	if err != nil || sess.User != "bram" {
		t.Fatalf("session %+v %v", sess, err)
	}

	// The session authenticates API requests; changes need the CSRF token.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/groups", nil)
	req.AddCookie(c)
	if p, fail := a.Authenticate(req); fail != nil || p.Name != "bram" || p.Machine {
		t.Fatalf("GET: %+v %v", p, fail)
	}
	req = httptest.NewRequest(http.MethodPut, "/api/v1/groups/x/hold", nil)
	req.AddCookie(c)
	if _, fail := a.Authenticate(req); fail == nil || fail.Status != http.StatusForbidden {
		t.Fatalf("PUT without CSRF: %v", fail)
	}
	req.Header.Set(CSRFHeader, sess.CSRF)
	if _, fail := a.Authenticate(req); fail != nil {
		t.Fatalf("PUT with CSRF: %v", fail)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if _, fail := a.Authenticate(req); fail == nil || fail.Status != http.StatusForbidden {
		t.Fatalf("cross-site PUT: %v", fail)
	}
}

func TestOIDCUserOutsideAllowedGroups(t *testing.T) {
	f := newFakeIssuer(t)
	f.groups = []string{"family"}
	resp := login(t, f, newAuthenticator(f, []string{"admins"}), false)
	if resp.StatusCode != http.StatusForbidden || sessionCookie(resp) != nil {
		t.Fatalf("status %d, cookie %v", resp.StatusCode, sessionCookie(resp))
	}
}

func TestOIDCStateMismatch(t *testing.T) {
	f := newFakeIssuer(t)
	resp := login(t, f, newAuthenticator(f, []string{"admins"}), true)
	if resp.StatusCode != http.StatusBadRequest || sessionCookie(resp) != nil {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestOIDCSubjectMismatch(t *testing.T) {
	f := newFakeIssuer(t)
	f.idTokenSub = "someone-else"
	resp := login(t, f, newAuthenticator(f, []string{"admins"}), false)
	if resp.StatusCode != http.StatusBadGateway || sessionCookie(resp) != nil {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestOIDCIssuerDown(t *testing.T) {
	a := &Authenticator{
		OIDC:   NewOIDC(OIDCConfig{IssuerURL: "http://127.0.0.1:1", ClientID: "standby", RedirectURL: "https://x/auth/callback"}),
		Signer: NewSigner(nil, nil), Log: slog.New(slog.DiscardHandler),
	}
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestBearerAuthentication(t *testing.T) {
	api := kubetest.New(t)
	api.TokenReview = func(token string, auds []string) (bool, string, []string) {
		if token == "good" {
			return true, "system:serviceaccount:ci:ci", auds
		}
		return false, "", nil
	}
	a := &Authenticator{
		Signer:   NewSigner(nil, nil),
		Reviewer: NewTokenReviewer(api.Client(), "aud", []string{"system:serviceaccount:ci:ci"}, nil),
		Log:      slog.New(slog.DiscardHandler),
	}
	for header, want := range map[string]int{
		"":            http.StatusUnauthorized,
		"Bearer bad":  http.StatusUnauthorized,
		"Basic abc":   http.StatusUnauthorized,
		"Bearer ":     http.StatusUnauthorized,
		"Bearer good": 0,
	} {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/groups/x/leases/y", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		p, fail := a.Authenticate(req)
		switch {
		case want == 0 && (fail != nil || !p.Machine):
			t.Errorf("%q: %+v %v", header, p, fail)
		case want != 0 && (fail == nil || fail.Status != want):
			t.Errorf("%q: %v, want %d", header, fail, want)
		}
	}
}
