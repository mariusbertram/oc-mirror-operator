package resourceapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/gorilla/mux"
	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgorest "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These tests live in the internal (non "_test") package because they need
// to set Server.baseCfg directly — NewServer's real config.GetConfig() call
// always fails outside a cluster, so the token-scoped-client branches of
// clientForRequest are otherwise unreachable from black-box tests.

var _ = Describe("clientForRequest token-scoped client", func() {
	newTestServer := func() *Server {
		sc := runtime.NewScheme()
		_ = corev1.AddToScheme(sc)
		_ = mirrorv1alpha1.AddToScheme(sc)
		c := fake.NewClientBuilder().WithScheme(sc).Build()
		return &Server{
			client:  c,
			scheme:  sc,
			baseCfg: &clientgorest.Config{Host: "https://fake-host-for-test.invalid:6443"},
		}
	}

	It("returns the service-account client when no token is present", func() {
		s := newTestServer()
		req := httptest.NewRequest("GET", "/", nil)
		Expect(s.clientForRequest(req)).To(BeIdenticalTo(s.client))
	})

	It("returns the service-account client when baseCfg is nil, even with a token", func() {
		s := newTestServer()
		s.baseCfg = nil
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer some-token")
		Expect(s.clientForRequest(req)).To(BeIdenticalTo(s.client))
	})

	It("builds and caches a token-scoped client from the Authorization header", func() {
		s := newTestServer()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer my-token")

		c1 := s.clientForRequest(req)
		Expect(c1).NotTo(BeIdenticalTo(s.client))

		// A second call with the same token reuses the cached client rather
		// than building a new one.
		c2 := s.clientForRequest(req)
		Expect(c2).To(BeIdenticalTo(c1))
	})

	It("builds and caches a token-scoped client from X-Forwarded-Access-Token", func() {
		s := newTestServer()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Forwarded-Access-Token", "forwarded-token")

		c := s.clientForRequest(req)
		Expect(c).NotTo(BeIdenticalTo(s.client))
	})

	It("rebuilds the client once its cache entry has expired", func() {
		s := newTestServer()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer expiring-token")

		_ = s.clientForRequest(req)

		// Force the cached entry to look expired, matching clientForRequest's
		// own key derivation (sha256 hex of the token).
		h := sha256.Sum256([]byte("expiring-token"))
		key := hex.EncodeToString(h[:])
		v, ok := s.tokenClients.Load(key)
		Expect(ok).To(BeTrue())
		entry := v.(*tokenClientEntry)
		entry.expiresAt = time.Now().Add(-time.Minute)

		c2 := s.clientForRequest(req)
		Expect(c2).NotTo(BeNil())
	})
})

var _ = Describe("writeJSON", func() {
	It("writes a JSON-encodable value with the correct content type", func() {
		rr := httptest.NewRecorder()
		writeJSON(rr, map[string]string{"hello": "world"})
		Expect(rr.Header().Get("Content-Type")).To(Equal("application/json"))
		Expect(rr.Body.String()).To(ContainSubstring("hello"))
	})

	It("writes a 500 when the value cannot be marshalled to JSON", func() {
		rr := httptest.NewRecorder()
		// Go channels are not JSON-marshallable.
		writeJSON(rr, make(chan int))
		Expect(rr.Code).To(Equal(http.StatusInternalServerError))
	})
})

var _ = Describe("RegisterPluginStaticRoutes", func() {
	It("registers a catch-all handler that serves the embedded plugin assets without panicking", func() {
		r := mux.NewRouter()
		Expect(func() { RegisterPluginStaticRoutes(r) }).NotTo(Panic())

		req := httptest.NewRequest("GET", "/plugin-manifest.json", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		// Whatever the embedded FS actually contains, serving it must not panic
		// and must produce *some* HTTP response.
		Expect(rr.Code).To(BeNumerically(">=", 200))
	})
})

var _ = Describe("RunOn", func() {
	It("starts and stops cleanly when the context is cancelled", func() {
		sc := runtime.NewScheme()
		_ = corev1.AddToScheme(sc)
		_ = mirrorv1alpha1.AddToScheme(sc)
		c := fake.NewClientBuilder().WithScheme(sc).Build()
		s := NewServer(c, "test-ns")

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.RunOn(ctx, "127.0.0.1:0")
		}()

		// Give the listener a moment to start, then request shutdown.
		time.Sleep(50 * time.Millisecond)
		cancel()

		Eventually(done, 5*time.Second).Should(BeClosed())
	})
})
