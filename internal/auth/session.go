package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Cookie names. The __Host- prefix makes browsers require Secure, Path=/
// and no Domain, so no other host can set or read them.
const (
	SessionCookie = "__Host-standby-session"
	LoginCookie   = "__Host-standby-login"
)

// Session is a signed-in person.
type Session struct {
	User string `json:"u"`
	// CSRF is the token browser requests must echo in X-CSRF-Token.
	CSRF    string `json:"c"`
	Expires int64  `json:"e"`
}

// loginState is kept in a short-lived cookie between /auth/login and
// /auth/callback.
type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
	Expires  int64  `json:"e"`
}

// Signer signs and verifies cookie values with HMAC-SHA256.
type Signer struct {
	key []byte
	now func() time.Time
}

// NewSigner returns a signer with key, or with a random key when key is nil
// (sessions then end when the process restarts).
func NewSigner(key []byte, now func() time.Time) *Signer {
	if key == nil {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic("crypto/rand: " + err.Error())
		}
	}
	if now == nil {
		now = time.Now
	}
	return &Signer{key: key, now: now}
}

var errInvalidCookie = errors.New("invalid or expired cookie")

// encode returns base64(json(v)) "." base64(hmac(purpose, payload)).
func (s *Signer) encode(purpose string, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("auth: encoding cookie: " + err.Error())
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(purpose, payload))
}

func (s *Signer) decode(purpose, value string, v any) error {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return errInvalidCookie
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(purpose, payload)) {
		return errInvalidCookie
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return errInvalidCookie
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errInvalidCookie
	}
	return nil
}

func (s *Signer) mac(purpose, payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(purpose))
	m.Write([]byte{0})
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// NewSession returns a session for user that lasts ttl.
func (s *Signer) NewSession(user string, ttl time.Duration) Session {
	return Session{User: user, CSRF: randomString(), Expires: s.now().Add(ttl).Unix()}
}

// EncodeSession returns the signed cookie value of sess.
func (s *Signer) EncodeSession(sess Session) string { return s.encode("session", sess) }

// DecodeSession verifies a session cookie value.
func (s *Signer) DecodeSession(value string) (Session, error) {
	var sess Session
	if err := s.decode("session", value, &sess); err != nil {
		return Session{}, err
	}
	if sess.User == "" || sess.CSRF == "" || s.now().Unix() >= sess.Expires {
		return Session{}, errInvalidCookie
	}
	return sess, nil
}

func (s *Signer) encodeLogin(l loginState) string { return s.encode("login", l) }

func (s *Signer) decodeLogin(value string) (loginState, error) {
	var l loginState
	if err := s.decode("login", value, &l); err != nil {
		return loginState{}, err
	}
	if s.now().Unix() >= l.Expires {
		return loginState{}, errInvalidCookie
	}
	return l, nil
}

// SessionFrom returns the valid session of a request, if any.
func (s *Signer) SessionFrom(r *http.Request) (Session, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return Session{}, false
	}
	sess, err := s.DecodeSession(c.Value)
	return sess, err == nil
}

func setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func randomString() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
