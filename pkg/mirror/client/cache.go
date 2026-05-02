package client

import (
	"sync"
	"time"
)

// ClientCache provides pooled access to MirrorClient instances keyed by authConfigPath.
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

// GetOrCreate returns a cached MirrorClient for the given authConfigPath, creating
// one if necessary. Clients are automatically refreshed after the configured interval.
func (cc *ClientCache) GetOrCreate(insecureHosts []string, authConfigPath string) (*MirrorClient, error) {
	key := authConfigPath

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

// Close closes all cached clients.
func (cc *ClientCache) Close() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.cache = make(map[string]*MirrorClient)
	cc.lastRefresh = make(map[string]time.Time)
}

// Reset clears the cache and all refresh times.
func (cc *ClientCache) Reset() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.cache = make(map[string]*MirrorClient)
	cc.lastRefresh = make(map[string]time.Time)
}

// RefreshClient forces a refresh of the cached client for the given authConfigPath.
// This is useful when auth config has changed.
func (cc *ClientCache) RefreshClient(insecureHosts []string, authConfigPath string) (*MirrorClient, error) {
	key := authConfigPath

	cc.mu.Lock()
	client := NewMirrorClient(insecureHosts, authConfigPath)
	cc.cache[key] = client
	cc.lastRefresh[key] = time.Now()
	cc.mu.Unlock()

	return client, nil
}
