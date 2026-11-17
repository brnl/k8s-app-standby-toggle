// Package auth authenticates people (OpenID Connect, signed session cookie,
// CSRF protection) and machines (Kubernetes ServiceAccount tokens checked
// with a TokenReview).
package auth

import (
	"crypto/subtle"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// CSRFHeader carries the session's CSRF token on state-changing browser
// requests.
const CSRFHeader = "X-CSRF-Token"

// CSRFFormField carries the CSRF token in plain HTML form posts.
const CSRFFormField = "csrf_token"

const loginTTL = 10 * time.Minute

// Principal is an authenticated caller.
type Principal struct {
	// Name is the username of a person or the ServiceAccount of a machine
	// (system:serviceaccount:<namespace>:<name>).
	Name    string
	Machine bool
	// CSRF is the session's CSRF token (people only).
	CSRF string
}

// Authenticator identifies callers.
type Authenticator struct {
	// Disabled lets every request in as an anonymous person (local
	// development only; config enforces a loopback listener).
	Disabled   bool
	OIDC       *OIDC
	Signer     *Signer
	Reviewer   *TokenReviewer
	SessionTTL time.Duration
	Log        *slog.Logger

	cop *http.CrossOriginProtection
}

// Failure is an authentication failure and the status to answer with.
type Failure struct {
	Status  int
	Message string
}

func (f *Failure) Error() string { return f.Message }

func (a *Authenticator) crossOrigin() *http.CrossOriginProtection {
	if a.cop == nil {
		a.cop = http.NewCrossOriginProtection()
	}
	return a.cop
}

// Authenticate identifies the caller of an API request. A request with an
// Authorization: Bearer header is a machine; anything else must carry a
// session cookie. State-changing requests with a cookie must pass the
// cross-origin check and carry the session's CSRF token.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, *Failure) {
	unsafe := !(r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions)
	if a.Disabled {
		if unsafe {
			if err := a.crossOrigin().Check(r); err != nil {
				return Principal{}, &Failure{http.StatusForbidden, "cross-origin request rejected"}
			}
		}
		return Principal{Name: "anonymous"}, nil
	}
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, token, ok := strings.Cut(h, " ")
		token = strings.TrimSpace(token)
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
			return Principal{}, &Failure{http.StatusUnauthorized, "the Authorization header must hold a Bearer token"}
		}
		user, err := a.Reviewer.Review(r.Context(), token)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			return Principal{}, &Failure{http.StatusUnauthorized, "invalid or expired token (it must be a ServiceAccount token for this service's audience)"}
		case errors.Is(err, ErrForbiddenMember):
			a.Log.Warn("ServiceAccount not allowed", "subject", user, "method", r.Method, "path", r.URL.Path)
			return Principal{}, &Failure{http.StatusForbidden, user + " is not allowed to use this service"}
		case err != nil:
			a.Log.Error("cannot verify token", "error", err.Error())
			return Principal{}, &Failure{http.StatusServiceUnavailable, "cannot verify the token right now"}
		}
		return Principal{Name: user, Machine: true}, nil
	}
	sess, ok := a.Signer.SessionFrom(r)
	if !ok {
		return Principal{}, &Failure{http.StatusUnauthorized, "not signed in"}
	}
	if unsafe {
		if err := a.crossOrigin().Check(r); err != nil {
			return Principal{}, &Failure{http.StatusForbidden, "cross-origin request rejected"}
		}
		token := r.Header.Get(CSRFHeader)
		if token == "" && r.Method == http.MethodPost {
			token = r.PostFormValue(CSRFFormField) // plain HTML forms (logout)
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRF)) != 1 {
			return Principal{}, &Failure{http.StatusForbidden, "missing or invalid CSRF token"}
		}
	}
	return Principal{Name: sess.User, CSRF: sess.CSRF}, nil
}

// Person returns the signed-in person of a page request.
func (a *Authenticator) Person(r *http.Request) (Principal, bool) {
	if a.Disabled {
		return Principal{Name: "anonymous"}, true
	}
	sess, ok := a.Signer.SessionFrom(r)
	if !ok {
		return Principal{}, false
	}
	return Principal{Name: sess.User, CSRF: sess.CSRF}, true
}

// LoginURL is where a page redirects a visitor who is not signed in.
func LoginURL(returnTo string) string {
	return "/auth/login?return=" + url.QueryEscape(returnTo)
}

// safeReturn accepts only local absolute paths.
func safeReturn(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") || strings.ContainsAny(p, "\r\n") {
		return "/"
	}
	return p
}

// Login starts the authorization code flow.
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	if a.Disabled {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	l := loginState{
		State:    randomString(),
		Nonce:    randomString(),
		Verifier: oauth2.GenerateVerifier(),
		Return:   safeReturn(r.URL.Query().Get("return")),
		Expires:  a.Signer.now().Add(loginTTL).Unix(),
	}
	target, err := a.OIDC.AuthCodeURL(r.Context(), l.State, l.Nonce, l.Verifier)
	if err != nil {
		a.Log.Error("cannot start login", "error", err.Error())
		writePage(w, http.StatusServiceUnavailable, "Sign-in unavailable", "The identity provider cannot be reached right now. Try again in a minute.", "/auth/login?return="+url.QueryEscape(l.Return), "Try again")
		return
	}
	setCookie(w, LoginCookie, a.Signer.encodeLogin(l), loginTTL)
	http.Redirect(w, r, target, http.StatusFound)
}

// Callback completes the authorization code flow.
func (a *Authenticator) Callback(w http.ResponseWriter, r *http.Request) {
	if a.Disabled {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	c, err := r.Cookie(LoginCookie)
	clearCookie(w, LoginCookie)
	if err != nil {
		writePage(w, http.StatusBadRequest, "Sign-in expired", "The sign-in took too long or was started elsewhere.", "/auth/login", "Sign in")
		return
	}
	l, err := a.Signer.decodeLogin(c.Value)
	if err != nil {
		writePage(w, http.StatusBadRequest, "Sign-in expired", "The sign-in took too long or was started elsewhere.", "/auth/login", "Sign in")
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		msg := e
		if d := q.Get("error_description"); d != "" {
			msg += ": " + d
		}
		a.Log.Warn("identity provider refused sign-in", "error", msg)
		writePage(w, http.StatusForbidden, "Sign-in refused", "The identity provider answered: "+msg, "/auth/login", "Try again")
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(l.State)) != 1 || q.Get("code") == "" {
		writePage(w, http.StatusBadRequest, "Sign-in failed", "The answer from the identity provider does not belong to this sign-in.", "/auth/login", "Sign in")
		return
	}
	id, err := a.OIDC.Exchange(r.Context(), q.Get("code"), l.Verifier, l.Nonce)
	switch {
	case errors.Is(err, ErrNotAllowed):
		a.Log.Warn("user not in an allowed group", "user", id.User, "groups", strings.Join(id.Groups, ","))
		writePage(w, http.StatusForbidden, "Access denied", "You are signed in as "+id.User+", but you are not a member of a group that may use this service.", "", "")
		return
	case err != nil:
		a.Log.Error("sign-in failed", "error", err.Error())
		writePage(w, http.StatusBadGateway, "Sign-in failed", "The sign-in could not be completed. Try again.", "/auth/login", "Sign in")
		return
	}
	sess := a.Signer.NewSession(id.User, a.SessionTTL)
	setCookie(w, SessionCookie, a.Signer.EncodeSession(sess), a.SessionTTL)
	a.Log.Info("user signed in", "user", id.User, "groups", strings.Join(id.Groups, ","))
	http.Redirect(w, r, l.Return, http.StatusFound)
}

// Logout ends the session. It is a POST protected like any other
// state-changing request.
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	p, fail := a.Authenticate(r)
	if fail != nil && fail.Status != http.StatusUnauthorized {
		http.Error(w, fail.Message, fail.Status)
		return
	}
	clearCookie(w, SessionCookie)
	if fail == nil {
		a.Log.Info("user signed out", "user", p.Name)
	}
	http.Redirect(w, r, "/auth/signed-out", http.StatusSeeOther)
}

// SignedOut confirms the logout.
func (a *Authenticator) SignedOut(w http.ResponseWriter, _ *http.Request) {
	writePage(w, http.StatusOK, "Signed out", "You are signed out of k8s-app-standby-toggle.", "/auth/login", "Sign in again")
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · k8s-app-standby-toggle</title><link rel="stylesheet" href="/static/style.css"></head>
<body><main class="notice"><h1>{{.Title}}</h1><p>{{.Message}}</p>{{if .Link}}<p><a class="button" href="{{.Link}}">{{.LinkText}}</a></p>{{end}}</main></body></html>
`))

func writePage(w http.ResponseWriter, status int, title, message, link, linkText string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, map[string]string{"Title": title, "Message": message, "Link": link, "LinkText": linkText})
}
