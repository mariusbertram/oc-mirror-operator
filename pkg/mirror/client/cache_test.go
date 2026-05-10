package client

import (
	"testing"
	"time"
)

func TestNewClientCache(t *testing.T) {
	cc := NewClientCache()
	if cc == nil {
		t.Fatal("expected non-nil ClientCache")
	}
	if cc.refreshAfter != 5*time.Minute {
		t.Errorf("expected refreshAfter=5m, got %v", cc.refreshAfter)
	}
	if cc.cache == nil {
		t.Error("expected initialized cache map")
	}
	if cc.lastRefresh == nil {
		t.Error("expected initialized lastRefresh map")
	}
}

func TestNewClientCacheWithInterval(t *testing.T) {
	d := 10 * time.Second
	cc := NewClientCacheWithInterval(d)
	if cc.refreshAfter != d {
		t.Errorf("expected refreshAfter=%v, got %v", d, cc.refreshAfter)
	}
}

func TestGetOrCreate_CreatesClientOnFirstCall(t *testing.T) {
	cc := NewClientCacheWithInterval(5 * time.Minute)
	c, err := cc.GetOrCreate(nil, "/tmp/auth.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestGetOrCreate_ReturnsCachedClient(t *testing.T) {
	cc := NewClientCacheWithInterval(5 * time.Minute)
	c1, _ := cc.GetOrCreate(nil, "/tmp/auth.json")
	c2, _ := cc.GetOrCreate(nil, "/tmp/auth.json")
	if c1 != c2 {
		t.Error("expected same cached client to be returned on second call")
	}
}

func TestGetOrCreate_RefreshesExpiredClient(t *testing.T) {
	cc := NewClientCacheWithInterval(1 * time.Millisecond)
	c1, _ := cc.GetOrCreate(nil, "/tmp/auth.json")
	time.Sleep(5 * time.Millisecond)
	c2, _ := cc.GetOrCreate(nil, "/tmp/auth.json")
	if c1 == c2 {
		t.Error("expected refreshed (new) client after TTL expiry")
	}
}

func TestGetOrCreate_DifferentKeysDontShare(t *testing.T) {
	cc := NewClientCacheWithInterval(5 * time.Minute)
	c1, _ := cc.GetOrCreate(nil, "/tmp/auth-a.json")
	c2, _ := cc.GetOrCreate(nil, "/tmp/auth-b.json")
	if c1 == c2 {
		t.Error("expected different client instances for different auth paths")
	}
}

func TestRefreshClient_ReplacesExistingEntry(t *testing.T) {
	cc := NewClientCacheWithInterval(5 * time.Minute)
	c1, _ := cc.GetOrCreate(nil, "/tmp/auth.json")
	c2, err := cc.RefreshClient(nil, "/tmp/auth.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c1 == c2 {
		t.Error("expected RefreshClient to return a new client instance")
	}
	// Subsequent GetOrCreate should return the freshly created client.
	c3, _ := cc.GetOrCreate(nil, "/tmp/auth.json")
	if c2 != c3 {
		t.Error("expected GetOrCreate to return the refreshed client")
	}
}

func TestRefreshClient_CreatesEntryWhenMissing(t *testing.T) {
	cc := NewClientCacheWithInterval(5 * time.Minute)
	c, err := cc.RefreshClient(nil, "/tmp/never-seen.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}
