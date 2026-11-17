// Package kubetest provides a fake Kubernetes API server for tests. It stores
// objects as JSON by path and implements get, list, JSON merge patch, create
// and update with resourceVersion checks, and TokenReview.
package kubetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/brnl/k8s-app-standby-toggle/internal/kube"
)

// Request records one request the server received.
type Request struct {
	Method string
	Path   string
	Body   string
}

// Server is a fake API server.
type Server struct {
	srv *httptest.Server

	mu       sync.Mutex
	objects  map[string]map[string]any
	rv       int
	requests []Request

	// TokenReview answers TokenReviews; nil rejects every token.
	TokenReview func(token string, audiences []string) (authenticated bool, username string, tokenAudiences []string)
	// Intercept, when set, can answer a request before the store does: it
	// returns a status code and true to fail the request with that code.
	Intercept func(method, path string) (int, bool)
}

// New starts a fake API server that is closed when the test ends.
func New(t testing.TB) *Server {
	s := &Server{objects: map[string]map[string]any{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the server's base URL.
func (s *Server) URL() string { return s.srv.URL }

// Client returns a kube client for the server.
func (s *Server) Client() *kube.Client {
	return kube.New(s.srv.URL, s.srv.Client(), nil, "kubetest")
}

// Set stores obj at path (replacing any object there) and returns its new
// resourceVersion. obj may be a map or any JSON-encodable value.
func (s *Server) Set(path string, obj any) string {
	m := toMap(obj)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store(path, m)
}

// Object returns a copy of the object at path, or nil.
func (s *Server) Object(path string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.objects[path]; ok {
		return deepCopy(o).(map[string]any)
	}
	return nil
}

// Delete removes the object at path.
func (s *Server) Delete(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, path)
}

// Requests returns the requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// ResetRequests clears the request log.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// Count returns how many requests with method had a path with prefix.
func (s *Server) Count(method, prefix string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method == method && strings.HasPrefix(r.Path, prefix) {
			n++
		}
	}
	return n
}

func (s *Server) store(path string, m map[string]any) string {
	s.rv++
	rv := strconv.Itoa(s.rv)
	meta, _ := m["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		m["metadata"] = meta
	}
	meta["resourceVersion"] = rv
	s.objects[path] = m
	return rv
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	s.mu.Unlock()

	if s.Intercept != nil {
		if code, ok := s.Intercept(r.Method, r.URL.Path); ok {
			writeStatus(w, code, http.StatusText(code), "intercepted")
			return
		}
	}

	path := r.URL.Path
	switch r.Method {
	case http.MethodGet:
		s.get(w, path)
	case http.MethodPatch:
		if ct := r.Header.Get("Content-Type"); ct != kube.MergePatch {
			writeStatus(w, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "only merge patches are supported, got "+ct)
			return
		}
		s.patch(w, path, body)
	case http.MethodPut:
		s.update(w, path, body)
	case http.MethodPost:
		if strings.HasSuffix(path, "/tokenreviews") {
			s.tokenReview(w, body)
			return
		}
		s.create(w, path, body)
	default:
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

// isCollection reports whether path names a collection of namespaced
// objects rather than an object.
func isCollection(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch parts[0] {
	case "api":
		return len(parts) == 5
	case "apis":
		return len(parts) == 6
	}
	return false
}

func (s *Server) get(w http.ResponseWriter, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if isCollection(path) {
		var keys []string
		for p := range s.objects {
			if rest, ok := strings.CutPrefix(p, path+"/"); ok && !strings.Contains(rest, "/") {
				keys = append(keys, p)
			}
		}
		sort.Strings(keys)
		items := []any{}
		for _, k := range keys {
			items = append(items, s.objects[k])
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "metadata": map[string]any{"resourceVersion": strconv.Itoa(s.rv)}})
		return
	}
	o, ok := s.objects[path]
	if !ok {
		writeStatus(w, http.StatusNotFound, "NotFound", path+" not found")
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) patch(w http.ResponseWriter, path string, body []byte) {
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[path]
	if !ok {
		writeStatus(w, http.StatusNotFound, "NotFound", path+" not found")
		return
	}
	if want := nestedString(p, "metadata", "resourceVersion"); want != "" && want != nestedString(o, "metadata", "resourceVersion") {
		writeStatus(w, http.StatusConflict, "Conflict", "the object has been modified")
		return
	}
	merged := MergePatch(deepCopy(o), p).(map[string]any)
	s.store(path, merged)
	writeJSON(w, http.StatusOK, merged)
}

func (s *Server) update(w http.ResponseWriter, path string, body []byte) {
	var o map[string]any
	if err := json.Unmarshal(body, &o); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.objects[path]
	if !ok {
		writeStatus(w, http.StatusNotFound, "NotFound", path+" not found")
		return
	}
	if want := nestedString(o, "metadata", "resourceVersion"); want != "" && want != nestedString(cur, "metadata", "resourceVersion") {
		writeStatus(w, http.StatusConflict, "Conflict", "the object has been modified")
		return
	}
	s.store(path, o)
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) create(w http.ResponseWriter, collection string, body []byte) {
	var o map[string]any
	if err := json.Unmarshal(body, &o); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	name := nestedString(o, "metadata", "name")
	if name == "" {
		writeStatus(w, http.StatusUnprocessableEntity, "Invalid", "metadata.name is required")
		return
	}
	path := collection + "/" + name
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[path]; ok {
		writeStatus(w, http.StatusConflict, "AlreadyExists", name+" already exists")
		return
	}
	s.store(path, o)
	writeJSON(w, http.StatusCreated, o)
}

func (s *Server) tokenReview(w http.ResponseWriter, body []byte) {
	var tr kube.TokenReview
	if err := json.Unmarshal(body, &tr); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	if s.TokenReview != nil {
		ok, user, auds := s.TokenReview(tr.Spec.Token, tr.Spec.Audiences)
		tr.Status.Authenticated = ok
		if ok {
			tr.Status.User.Username = user
			tr.Status.Audiences = auds
		}
	}
	if !tr.Status.Authenticated {
		tr.Status.Error = "invalid bearer token"
	}
	tr.Spec.Token = ""
	writeJSON(w, http.StatusCreated, tr)
}

// MergePatch applies a JSON merge patch (RFC 7386) to target.
func MergePatch(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	t, ok := target.(map[string]any)
	if !ok {
		t = map[string]any{}
	}
	for k, v := range p {
		if v == nil {
			delete(t, k)
			continue
		}
		t[k] = MergePatch(t[k], v)
	}
	return t
}

func nestedString(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

func toMap(obj any) map[string]any {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(fmt.Sprintf("kubetest: encoding object: %v", err))
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		panic(fmt.Sprintf("kubetest: object is not a JSON object: %v", err))
	}
	return m
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = deepCopy(x)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = deepCopy(x)
		}
		return s
	default:
		return v
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, code int, reason, message string) {
	writeJSON(w, code, map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure",
		"reason": reason, "message": message, "code": code,
	})
}
