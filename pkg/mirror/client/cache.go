package client

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// ClientCache provides pooled access to MirrorClient instances keyed by
// authConfigPath and insecure-host set.
// This reduces connection churn and token scope accumulation issues (e.g., Quay's
// nginx proxy rejecting tokens > ~8 KB). Cached clients are refreshed every 5 minutes.
type ClientCache struct {
	cache        map[string]*MirrorClient
	mu           sync.RWMutex
	lastRefresh  map[string]time.Time
	refreshAfter time.Duration
}

// NewClientCache creates a new ClientCache with a default refresh interval of 5 minutes.
func NewClientCache() *ClientCache {
	return NewClientCacheWithInterval(5 * time.Minute)
}

// NewClientCacheWithInterval creates a new ClientCache with a custom refresh interval.
func NewClientCacheWithInterval(refreshAfter time.Duration) *ClientCache {
	return &ClientCache{
		cache:        make(map[string]*MirrorClient),
		lastRefresh:  make(map[string]time.Time),
		refreshAfter: refreshAfter,
	}
}

// cacheKey identifies a client by its credentials and its insecure hosts, so
// a caller asking for a different insecure-host set never gets a client built
// for another one (e.g. an HTTPS-only client for an insecure HTTP registry).
func cacheKey(insecureHosts []string, authConfigPath string) string {
	hosts := slices.Clone(insecureHosts)
	slices.Sort(hosts)
	return authConfigPath + "\x00" + strings.Join(hosts, ",")
}

// GetOrCreate returns a cached MirrorClient for the given insecure hosts and
// authConfigPath, creating one if necessary. Clients are automatically
// refreshed after the configured interval.
func (cc *ClientCache) GetOrCreate(insecureHosts []string, authConfigPath string) (*MirrorClient, error) {
	key := cacheKey(insecureHosts, authConfigPath)

	cc.mu.Lock()
	defer cc.mu.Unlock()

	if existing, ok := cc.cache[key]; ok && time.Since(cc.lastRefresh[key]) < cc.refreshAfter {
		return existing, nil
	}

	client := NewMirrorClient(insecureHosts, authConfigPath)
	cc.cache[key] = client
	cc.lastRefresh[key] = time.Now()
	return client, nil
}

// RefreshClient forces a refresh of the cached client for the given insecure
// hosts and authConfigPath. This is useful when auth config has changed.
func (cc *ClientCache) RefreshClient(insecureHosts []string, authConfigPath string) (*MirrorClient, error) {
	key := cacheKey(insecureHosts, authConfigPath)

	cc.mu.Lock()
	client := NewMirrorClient(insecureHosts, authConfigPath)
	cc.cache[key] = client
	cc.lastRefresh[key] = time.Now()
	cc.mu.Unlock()

	return client, nil
}
