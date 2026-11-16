package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"

	"github.com/brnl/k8s-app-standby-toggle/internal/kube"
)

// Store persists the state.
type Store interface {
	// Load returns the current state.
	Load(ctx context.Context) (*State, error)
	// Update applies fn to the current state and stores the result. fn may
	// run more than once (after a concurrent change); an error from fn
	// aborts the update and is returned as is.
	Update(ctx context.Context, fn func(*State) error) (*State, error)
}

// MemoryStore keeps the state in memory (tests and development).
type MemoryStore struct {
	mu sync.Mutex
	s  *State
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{s: New()} }

// Load implements Store.
func (m *MemoryStore) Load(context.Context) (*State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s.Clone(), nil
}

// Update implements Store.
func (m *MemoryStore) Update(_ context.Context, fn func(*State) error) (*State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.s.Clone()
	if err := fn(next); err != nil {
		return nil, err
	}
	next.Compact()
	m.s = next
	return next.Clone(), nil
}

// ConfigMapKey is the ConfigMap data key that holds the state.
const ConfigMapKey = "state.json"

const maxUpdateAttempts = 5

// ConfigMapStore keeps the state in one ConfigMap, which it creates when it
// does not exist. Updates use optimistic concurrency on resourceVersion.
type ConfigMapStore struct {
	client    *kube.Client
	namespace string
	name      string
	labels    map[string]string
}

// NewConfigMapStore returns a store backed by the ConfigMap namespace/name.
func NewConfigMapStore(client *kube.Client, namespace, name string) *ConfigMapStore {
	return &ConfigMapStore{
		client:    client,
		namespace: namespace,
		name:      name,
		labels: map[string]string{
			"app.kubernetes.io/name":       "k8s-app-standby-toggle",
			"app.kubernetes.io/component":  "state",
			"app.kubernetes.io/managed-by": "k8s-app-standby-toggle",
		},
	}
}

func (c *ConfigMapStore) path(name string) string {
	return kube.Path("", "v1", "configmaps", c.namespace, name)
}

// Load implements Store.
func (c *ConfigMapStore) Load(ctx context.Context) (*State, error) {
	s, _, err := c.get(ctx)
	return s, err
}

// get reads the ConfigMap, creating it when it is missing, and returns the
// decoded state with the ConfigMap it came from.
func (c *ConfigMapStore) get(ctx context.Context) (*State, *kube.ConfigMap, error) {
	var cm kube.ConfigMap
	err := c.client.Get(ctx, c.path(c.name), &cm)
	if kube.IsNotFound(err) {
		cm, err = c.create(ctx)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading state ConfigMap %s/%s: %w", c.namespace, c.name, err)
	}
	s := New()
	if raw := cm.Data[ConfigMapKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), s); err != nil {
			return nil, nil, fmt.Errorf("state ConfigMap %s/%s: key %s holds invalid JSON (fix or delete it): %w", c.namespace, c.name, ConfigMapKey, err)
		}
	}
	if s.Groups == nil {
		s.Groups = map[string]*Group{}
	}
	return s, &cm, nil
}

func (c *ConfigMapStore) create(ctx context.Context) (kube.ConfigMap, error) {
	cm := kube.ConfigMap{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: kube.ObjectMeta{
			Name:      c.name,
			Namespace: c.namespace,
			Labels:    maps.Clone(c.labels),
			Annotations: map[string]string{
				"k8s-app-standby-toggle.cloud64.net/description": "Runtime state of k8s-app-standby-toggle (overrides, leases, holds). Created and updated by the service.",
			},
		},
		Data: map[string]string{ConfigMapKey: encode(New())},
	}
	var out kube.ConfigMap
	err := c.client.Create(ctx, kube.Path("", "v1", "configmaps", c.namespace, ""), cm, &out)
	if kube.IsAlreadyExists(err) {
		err = c.client.Get(ctx, c.path(c.name), &out)
	}
	return out, err
}

// Update implements Store.
func (c *ConfigMapStore) Update(ctx context.Context, fn func(*State) error) (*State, error) {
	for range maxUpdateAttempts {
		cur, cm, err := c.get(ctx)
		if err != nil {
			return nil, err
		}
		before := encode(cur)
		if err := fn(cur); err != nil {
			return nil, err
		}
		cur.Compact()
		after := encode(cur)
		if after == before {
			return cur, nil
		}
		// Replace only the data; the resourceVersion read above makes the
		// update fail if anything changed in between.
		cm.APIVersion, cm.Kind = "v1", "ConfigMap"
		cm.Data = map[string]string{ConfigMapKey: after}
		err = c.client.Update(ctx, c.path(c.name), cm, nil)
		if kube.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("writing state ConfigMap %s/%s: %w", c.namespace, c.name, err)
		}
		return cur, nil
	}
	return nil, errors.New("writing state ConfigMap: too many concurrent changes, try again")
}

func encode(s *State) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		panic(fmt.Sprintf("state: encoding: %v", err))
	}
	return buf.String()
}
