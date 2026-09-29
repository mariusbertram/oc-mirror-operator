package client

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/regclient/regclient"
)

// A primary failure must survive a failed insecure-registry fallback instead
// of being replaced by the fallback's own (usually transport-level) error
// (#186), and no fallback may be attempted once the context is done.
func TestWithFallback(t *testing.T) {
	errPrimary := errors.New("primary: blob upload rejected")
	errFallback := errors.New("fallback: server gave HTTP response to HTTPS client")

	newClient := func(withFallback bool) *MirrorClient {
		c := &MirrorClient{rc: regclient.New()}
		if withFallback {
			c.rcFallback = regclient.New()
		}
		return c
	}
	// call returns primaryErr on the primary client and fallbackErr on the
	// fallback, counting the legs it ran.
	call := func(c *MirrorClient, primaryErr, fallbackErr error, legs *int) func(*regclient.RegClient) (string, error) {
		return func(rc *regclient.RegClient) (string, error) {
			*legs++
			if rc == c.rc {
				return "primary", primaryErr
			}
			return "fallback", fallbackErr
		}
	}

	t.Run("primary succeeds: no fallback", func(t *testing.T) {
		c, legs := newClient(true), 0
		v, err := withFallback(context.Background(), c, call(c, nil, errFallback, &legs))
		if err != nil || v != "primary" || legs != 1 {
			t.Errorf("got (%q, %v) after %d legs, want (primary, nil) after 1", v, err, legs)
		}
	})

	t.Run("fallback succeeds", func(t *testing.T) {
		c, legs := newClient(true), 0
		v, err := withFallback(context.Background(), c, call(c, errPrimary, nil, &legs))
		if err != nil || v != "fallback" || legs != 2 {
			t.Errorf("got (%q, %v) after %d legs, want (fallback, nil) after 2", v, err, legs)
		}
	})

	t.Run("both fail: both errors are kept", func(t *testing.T) {
		c, legs := newClient(true), 0
		_, err := withFallback(context.Background(), c, call(c, errPrimary, errFallback, &legs))
		if !errors.Is(err, errPrimary) || !errors.Is(err, errFallback) {
			t.Fatalf("expected both errors to be matchable, got %v", err)
		}
		if !strings.HasPrefix(err.Error(), errPrimary.Error()) {
			t.Errorf("expected the primary error to lead the message, got %q", err)
		}
	})

	t.Run("no fallback client: primary error unchanged", func(t *testing.T) {
		c, legs := newClient(false), 0
		_, err := withFallback(context.Background(), c, call(c, errPrimary, errFallback, &legs))
		if err != errPrimary || legs != 1 { //nolint:errorlint // identity is the point
			t.Errorf("got %v after %d legs, want the primary error after 1", err, legs)
		}
	})

	t.Run("context done: no fallback", func(t *testing.T) {
		c, legs := newClient(true), 0
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := withFallback(ctx, c, call(c, errPrimary, errFallback, &legs))
		if err != errPrimary || legs != 1 { //nolint:errorlint // identity is the point
			t.Errorf("got %v after %d legs, want the primary error after 1", err, legs)
		}
	})
}

// Clients for different insecure-host sets must not be shared: a caller
// asking for an insecure host must never get a client built without it.
func TestClientCache_KeyedByInsecureHosts(t *testing.T) {
	cc := NewClientCache()
	secure, _ := cc.GetOrCreate(nil, "/tmp/auth.json")
	insecure, _ := cc.GetOrCreate([]string{"reg.local:5000"}, "/tmp/auth.json")
	if secure == insecure {
		t.Fatal("expected different clients for different insecure-host sets")
	}
	if insecure.rcFallback == nil {
		t.Error("expected the insecure client to carry the HTTPS fallback")
	}
	again, _ := cc.GetOrCreate([]string{"reg.local:5000"}, "/tmp/auth.json")
	if again != insecure {
		t.Error("expected the same insecure-host set to hit the cache")
	}
	a, _ := cc.GetOrCreate([]string{"b", "a"}, "/tmp/auth.json")
	b, _ := cc.GetOrCreate([]string{"a", "b"}, "/tmp/auth.json")
	if a != b {
		t.Error("expected the insecure-host order not to matter")
	}
}
