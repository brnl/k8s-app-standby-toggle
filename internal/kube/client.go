// Package kube is a minimal Kubernetes API client built on net/http and
// encoding/json. It supports exactly what the service needs: get, list,
// merge patch, create and update, with the in-cluster ServiceAccount
// credentials.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	// MergePatch is the content type of a JSON merge patch (RFC 7386).
	MergePatch = "application/merge-patch+json"

	maxResponseBytes = 32 << 20
	tokenRefresh     = time.Minute
)

// Client talks to the Kubernetes API server.
type Client struct {
	base      string
	http      *http.Client
	token     func() (string, error)
	userAgent string
}

// New returns a client for the API server at baseURL. token may be nil (for
// example behind `kubectl proxy`).
func New(baseURL string, httpClient *http.Client, token func() (string, error), userAgent string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		base:      strings.TrimSuffix(baseURL, "/"),
		http:      httpClient,
		token:     token,
		userAgent: userAgent,
	}
}

// NewInCluster returns a client that uses the pod's ServiceAccount.
func NewInCluster(userAgent string) (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a Kubernetes pod (KUBERNETES_SERVICE_HOST is not set); set STANDBY_KUBE_API_URL for local development")
	}
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("reading the cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("the cluster CA file holds no certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // always talk to the API server directly
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return New(
		"https://"+net.JoinHostPort(host, port),
		&http.Client{Transport: transport, Timeout: 30 * time.Second},
		FileToken(serviceAccountDir+"/token"),
		userAgent,
	), nil
}

// FileToken returns a token source that reads path and re-reads it every
// minute, because the kubelet rotates projected ServiceAccount tokens.
func FileToken(path string) func() (string, error) {
	var (
		mu      sync.Mutex
		token   string
		fetched time.Time
	)
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if token != "" && time.Since(fetched) < tokenRefresh {
			return token, nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			if token != "" {
				return token, nil // keep using the last token
			}
			return "", fmt.Errorf("reading the ServiceAccount token: %w", err)
		}
		token, fetched = strings.TrimSpace(string(b)), time.Now()
		return token, nil
	}
}

// Error is a failed API request.
type Error struct {
	Method  string
	Path    string
	Code    int
	Reason  string
	Message string
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Code)
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Code, msg)
}

// IsNotFound reports whether err is a 404 from the API server.
func IsNotFound(err error) bool { return code(err) == http.StatusNotFound }

// IsConflict reports whether err is a 409 caused by a stale resourceVersion.
func IsConflict(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusConflict && e.Reason != "AlreadyExists"
}

// IsAlreadyExists reports whether err is a 409 because the object exists.
func IsAlreadyExists(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusConflict && e.Reason == "AlreadyExists"
}

// IsForbidden reports whether err is a 403 from the API server.
func IsForbidden(err error) bool { return code(err) == http.StatusForbidden }

func code(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// Get reads the object or list at path into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, "", nil, out)
}

// Patch applies a JSON merge patch and decodes the updated object into out.
func (c *Client) Patch(ctx context.Context, path string, patch, out any) error {
	return c.Do(ctx, http.MethodPatch, path, MergePatch, patch, out)
}

// Create posts obj to the collection at path.
func (c *Client) Create(ctx context.Context, path string, obj, out any) error {
	return c.Do(ctx, http.MethodPost, path, "application/json", obj, out)
}

// Update replaces the object at path. Set metadata.resourceVersion in obj for
// optimistic concurrency.
func (c *Client) Update(ctx context.Context, path string, obj, out any) error {
	return c.Do(ctx, http.MethodPut, path, "application/json", obj, out)
}

// Do performs one request. body is encoded as JSON when not nil.
func (c *Client) Do(ctx context.Context, method, path, contentType string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s %s: encoding body: %w", method, path, err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if c.token != nil {
		token, err := c.token()
		if err != nil {
			return err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &Error{Method: method, Path: path, Code: resp.StatusCode}
		var status struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &status) == nil {
			apiErr.Reason, apiErr.Message = status.Reason, status.Message
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decoding response: %w", method, path, err)
	}
	return nil
}

// Path builds the API path of a namespaced object, or of the collection when
// name is empty. group is "" for the core API.
func Path(group, version, resource, namespace, name string) string {
	var b strings.Builder
	if group == "" {
		b.WriteString("/api/" + version)
	} else {
		b.WriteString("/apis/" + group + "/" + version)
	}
	if namespace != "" {
		b.WriteString("/namespaces/" + url.PathEscape(namespace))
	}
	b.WriteString("/" + resource)
	if name != "" {
		b.WriteString("/" + url.PathEscape(name))
	}
	return b.String()
}
