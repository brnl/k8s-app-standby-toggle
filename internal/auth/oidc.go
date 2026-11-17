package auth

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig configures the OpenID Connect relying party.
type OIDCConfig struct {
	IssuerURL   string
	ClientID    string
	RedirectURL string
	// HTTPClient talks to the issuer (with its CA); nil uses a default.
	HTTPClient    *http.Client
	AllowedGroups []string
	AllowAnyUser  bool
}

// OIDC is a public OpenID Connect client that uses the authorization code
// flow with PKCE (S256), so it has no client secret.
type OIDC struct {
	cfg OIDCConfig

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// Scopes requested at login.
var Scopes = []string{oidc.ScopeOpenID, "profile", "groups"}

// ErrNotAllowed means the user is authenticated but not in an allowed group.
var ErrNotAllowed = errors.New("not in an allowed group")

// NewOIDC returns a relying party. The issuer is contacted on first use, so
// the service starts even while the identity provider is down.
func NewOIDC(cfg OIDCConfig) *OIDC {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &OIDC{cfg: cfg}
}

// IssuerHTTPClient returns an HTTP client that trusts the system roots and,
// if caFile is set, the certificates in it (for a self-signed issuer).
func IssuerHTTPClient(caFile string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("STANDBY_OIDC_CA_FILE: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("STANDBY_OIDC_CA_FILE: no PEM certificates found")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}, nil
}

func (o *OIDC) ctx(ctx context.Context) context.Context {
	ctx = oidc.ClientContext(ctx, o.cfg.HTTPClient)
	return context.WithValue(ctx, oauth2.HTTPClient, o.cfg.HTTPClient)
}

// init discovers the issuer once it is reachable.
func (o *OIDC) init(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider != nil {
		return nil
	}
	provider, err := oidc.NewProvider(o.ctx(ctx), o.cfg.IssuerURL)
	if err != nil {
		return fmt.Errorf("OIDC discovery at %s: %w", o.cfg.IssuerURL, err)
	}
	endpoint := provider.Endpoint()
	// A public client sends its client_id in the body and no secret.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	o.provider = provider
	o.verifier = provider.Verifier(&oidc.Config{ClientID: o.cfg.ClientID})
	o.oauth = &oauth2.Config{
		ClientID:    o.cfg.ClientID,
		Endpoint:    endpoint,
		RedirectURL: o.cfg.RedirectURL,
		Scopes:      Scopes,
	}
	return nil
}

// AuthCodeURL returns the issuer's authorization URL.
func (o *OIDC) AuthCodeURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	if err := o.init(ctx); err != nil {
		return "", err
	}
	return o.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), nil
}

// Identity is a signed-in user.
type Identity struct {
	User   string
	Groups []string
}

// Exchange redeems an authorization code, verifies the ID token and its
// nonce, reads the user's groups from the userinfo endpoint and checks them
// against the allowed groups. It returns ErrNotAllowed (with the identity)
// for a user outside the allowed groups.
func (o *OIDC) Exchange(ctx context.Context, code, verifier, nonce string) (Identity, error) {
	if err := o.init(ctx); err != nil {
		return Identity{}, err
	}
	ctx = o.ctx(ctx)
	token, err := o.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("exchanging the authorization code: %w", err)
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok || rawID == "" {
		return Identity{}, errors.New("the token response has no id_token")
	}
	idToken, err := o.verifier.Verify(ctx, rawID)
	if err != nil {
		return Identity{}, fmt.Errorf("verifying the ID token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return Identity{}, errors.New("the ID token nonce does not match")
	}
	if idToken.AccessTokenHash != "" {
		if err := idToken.VerifyAccessToken(token.AccessToken); err != nil {
			return Identity{}, fmt.Errorf("verifying the access token hash: %w", err)
		}
	}
	// Authelia does not put groups in the ID token by default: read them
	// from userinfo.
	info, err := o.provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err != nil {
		return Identity{}, fmt.Errorf("reading userinfo: %w", err)
	}
	if info.Subject != idToken.Subject {
		return Identity{}, errors.New("the userinfo subject does not match the ID token")
	}
	var claims struct {
		PreferredUsername string   `json:"preferred_username"`
		Name              string   `json:"name"`
		Email             string   `json:"email"`
		Groups            []string `json:"groups"`
	}
	if err := info.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("decoding userinfo: %w", err)
	}
	id := Identity{Groups: claims.Groups}
	for _, name := range []string{claims.PreferredUsername, claims.Email, claims.Name, info.Subject} {
		if name != "" {
			id.User = name
			break
		}
	}
	if !o.allowed(id.Groups) {
		return id, ErrNotAllowed
	}
	return id, nil
}

func (o *OIDC) allowed(groups []string) bool {
	if len(o.cfg.AllowedGroups) == 0 {
		return o.cfg.AllowAnyUser
	}
	for _, g := range groups {
		if slices.Contains(o.cfg.AllowedGroups, g) {
			return true
		}
	}
	return false
}
