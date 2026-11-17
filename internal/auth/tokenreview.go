package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/brnl/k8s-app-standby-toggle/internal/kube"
)

// Errors returned by TokenReviewer.Review.
var (
	ErrUnauthenticated = errors.New("invalid or expired token")
	ErrForbiddenMember = errors.New("subject not allowed")
)

const (
	reviewCacheTTL  = 30 * time.Second
	reviewCacheSize = 1024
)

// TokenReviewer authenticates ServiceAccount tokens with the Kubernetes
// TokenReview API and accepts only configured subjects.
type TokenReviewer struct {
	client   *kube.Client
	audience string
	subjects []string
	now      func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]reviewResult
}

type reviewResult struct {
	user    string
	err     error
	expires time.Time
}

// NewTokenReviewer returns a reviewer that requires tokens for audience and
// accepts the given subjects (system:serviceaccount:<ns>:<name>).
func NewTokenReviewer(client *kube.Client, audience string, subjects []string, now func() time.Time) *TokenReviewer {
	if now == nil {
		now = time.Now
	}
	return &TokenReviewer{client: client, audience: audience, subjects: subjects, now: now, cache: map[[32]byte]reviewResult{}}
}

// Review returns the subject of token. It returns ErrUnauthenticated for a
// token the API server rejects (or one for another audience), and
// ErrForbiddenMember (with the subject) for a subject that is not allowed.
// Results are cached briefly, so polling clients do not cause a TokenReview
// per request.
func (t *TokenReviewer) Review(ctx context.Context, token string) (string, error) {
	key := sha256.Sum256([]byte(token))
	now := t.now()
	t.mu.Lock()
	if r, ok := t.cache[key]; ok && now.Before(r.expires) {
		t.mu.Unlock()
		return r.user, r.err
	}
	t.mu.Unlock()

	var review kube.TokenReview
	review.APIVersion, review.Kind = "authentication.k8s.io/v1", "TokenReview"
	review.Spec.Token = token
	review.Spec.Audiences = []string{t.audience}
	var out kube.TokenReview
	if err := t.client.Create(ctx, "/apis/authentication.k8s.io/v1/tokenreviews", review, &out); err != nil {
		return "", fmt.Errorf("TokenReview failed: %w", err)
	}
	r := reviewResult{expires: now.Add(reviewCacheTTL)}
	switch {
	case !out.Status.Authenticated:
		r.err = ErrUnauthenticated
	case len(out.Status.Audiences) > 0 && !slices.Contains(out.Status.Audiences, t.audience):
		r.err = ErrUnauthenticated
	case !slices.Contains(t.subjects, out.Status.User.Username):
		r.user, r.err = out.Status.User.Username, ErrForbiddenMember
	default:
		r.user = out.Status.User.Username
	}

	t.mu.Lock()
	if len(t.cache) >= reviewCacheSize {
		for k, v := range t.cache {
			if !now.Before(v.expires) {
				delete(t.cache, k)
			}
		}
		if len(t.cache) >= reviewCacheSize {
			clear(t.cache)
		}
	}
	t.cache[key] = r
	t.mu.Unlock()
	return r.user, r.err
}
