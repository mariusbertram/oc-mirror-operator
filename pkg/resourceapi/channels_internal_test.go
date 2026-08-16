package resourceapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These tests live in the internal (non "_test") package because they need
// to override the unexported githubGraphDataChannelsURL var and construct a
// Server directly to control its unexported channelCache state.

var _ = Describe("fetchChannelsFromGitHub", func() {
	var origURL string

	BeforeEach(func() {
		origURL = githubGraphDataChannelsURL
	})
	AfterEach(func() {
		githubGraphDataChannelsURL = origURL
	})

	It("parses channel entries from the GitHub contents response", func() {
		body, _ := json.Marshal([]githubContentEntry{
			{Name: "stable-4.18.yaml", Type: "file"},
			{Name: "eus-4.16.yaml", Type: "file"},
			{Name: "okd-4.14.yaml", Type: "file"},
			{Name: "channels", Type: "dir"},            // skipped: not a file
			{Name: "README.md", Type: "file"},          // skipped: not .yaml
			{Name: "nodash.yaml", Type: "file"},        // skipped: no "-" in basename
			{Name: "stable-latest.yaml", Type: "file"}, // skipped: no "." in version part
		})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}))
		defer srv.Close()
		githubGraphDataChannelsURL = srv.URL

		s := &Server{}
		entries, err := s.fetchChannelsFromGitHub(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(ConsistOf(
			ocpChannelEntry{Name: "stable-4.18", Type: "ocp", Version: "4.18"},
			ocpChannelEntry{Name: "eus-4.16", Type: "ocp", Version: "4.16"},
			ocpChannelEntry{Name: "okd-4.14", Type: "okd", Version: "4.14"},
		))
	})

	It("returns an error on a non-200 response", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		githubGraphDataChannelsURL = srv.URL

		s := &Server{}
		_, err := s.fetchChannelsFromGitHub(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("503"))
	})

	It("returns an error on malformed JSON", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		githubGraphDataChannelsURL = srv.URL

		s := &Server{}
		_, err := s.fetchChannelsFromGitHub(context.Background())
		Expect(err).To(HaveOccurred())
	})

	It("returns an error when no entries parse from the response", func() {
		body, _ := json.Marshal([]githubContentEntry{{Name: "README.md", Type: "file"}})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}))
		defer srv.Close()
		githubGraphDataChannelsURL = srv.URL

		s := &Server{}
		_, err := s.fetchChannelsFromGitHub(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no channel entries parsed"))
	})

	It("returns an error when the request context is already cancelled", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s := &Server{}
		_, err := s.fetchChannelsFromGitHub(ctx)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("fetchChannelsFromConfigMap", func() {
	const ns = "operator-ns"

	newServerWithCM := func(cm *corev1.ConfigMap) *Server {
		sc := runtime.NewScheme()
		_ = corev1.AddToScheme(sc)
		_ = mirrorv1alpha1.AddToScheme(sc)
		builder := fake.NewClientBuilder().WithScheme(sc)
		if cm != nil {
			builder = builder.WithObjects(cm)
		}
		return NewServer(builder.Build(), ns)
	}

	It("returns an error when no namespace is configured (cluster-wide mode)", func() {
		s := NewServerClusterWide(fake.NewClientBuilder().Build())
		_, err := s.fetchChannelsFromConfigMap(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no namespace configured"))
	})

	It("returns an error when the ConfigMap does not exist", func() {
		s := newServerWithCM(nil)
		_, err := s.fetchChannelsFromConfigMap(context.Background())
		Expect(err).To(HaveOccurred())
	})

	It("returns an error when the versions key is missing", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "oc-mirror-ocp-versions", Namespace: ns},
			Data:       map[string]string{},
		}
		s := newServerWithCM(cm)
		_, err := s.fetchChannelsFromConfigMap(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no 'versions' key"))
	})

	It("derives entries for every version and default channel type, skipping eus on odd minors", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "oc-mirror-ocp-versions", Namespace: ns},
			Data:       map[string]string{"versions": "4.16,4.17"},
		}
		s := newServerWithCM(cm)
		entries, err := s.fetchChannelsFromConfigMap(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(ContainElement(ocpChannelEntry{Name: "stable-4.16", Type: "ocp", Version: "4.16"}))
		Expect(entries).To(ContainElement(ocpChannelEntry{Name: "eus-4.16", Type: "ocp", Version: "4.16"}))
		Expect(entries).To(ContainElement(ocpChannelEntry{Name: "stable-4.17", Type: "ocp", Version: "4.17"}))
		Expect(entries).NotTo(ContainElement(ocpChannelEntry{Name: "eus-4.17", Type: "ocp", Version: "4.17"}))
	})

	It("respects a custom channelTypes list and marks okd entries", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "oc-mirror-ocp-versions", Namespace: ns},
			Data: map[string]string{
				"versions":     "4.18, ,4.19",
				"channelTypes": "stable, ,okd",
			},
		}
		s := newServerWithCM(cm)
		entries, err := s.fetchChannelsFromConfigMap(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(ContainElement(ocpChannelEntry{Name: "stable-4.18", Type: "ocp", Version: "4.18"}))
		Expect(entries).To(ContainElement(ocpChannelEntry{Name: "okd-4.18", Type: "okd", Version: "4.18"}))
		Expect(entries).NotTo(ContainElement(ocpChannelEntry{Name: "fast-4.18", Type: "ocp", Version: "4.18"}))
	})

	It("returns an error when no valid version entries are derivable", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "oc-mirror-ocp-versions", Namespace: ns},
			Data:       map[string]string{"versions": "garbage, ,"},
		}
		s := newServerWithCM(cm)
		_, err := s.fetchChannelsFromConfigMap(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no channel entries derived"))
	})
})

// unreachableGitHubURL stands in for a GitHub endpoint that can never be
// reached, proving that a code path did not depend on actually calling it.
const unreachableGitHubURL = "http://127.0.0.1:0"

var _ = Describe("getOCPChannels / handleGetOCPChannels", func() {
	var origURL string

	BeforeEach(func() {
		origURL = githubGraphDataChannelsURL
	})
	AfterEach(func() {
		githubGraphDataChannelsURL = origURL
	})

	It("returns the cached entries without re-fetching when the cache is fresh", func() {
		s := &Server{}
		want := []ocpChannelEntry{{Name: "cached-4.20", Type: "ocp", Version: "4.20"}}
		s.channelCache.entries = want
		s.channelCache.fetchedAt = time.Now()

		// Point at a URL that would error if actually called, proving the
		// cache short-circuits the fetch.
		githubGraphDataChannelsURL = unreachableGitHubURL

		got := s.getOCPChannels(context.Background())
		Expect(got).To(Equal(want))
	})

	It("fetches from GitHub on a cache miss and caches the result", func() {
		body, _ := json.Marshal([]githubContentEntry{{Name: "stable-4.18.yaml", Type: "file"}})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}))
		defer srv.Close()
		githubGraphDataChannelsURL = srv.URL

		s := &Server{}
		got := s.getOCPChannels(context.Background())
		Expect(got).To(ConsistOf(ocpChannelEntry{Name: "stable-4.18", Type: "ocp", Version: "4.18"}))
		Expect(s.channelCache.entries).To(Equal(got))
		Expect(s.channelCache.fetchedAt).NotTo(BeZero())
	})

	It("falls back to the ConfigMap when GitHub is unreachable", func() {
		githubGraphDataChannelsURL = unreachableGitHubURL

		sc := runtime.NewScheme()
		_ = corev1.AddToScheme(sc)
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "oc-mirror-ocp-versions", Namespace: "op-ns"},
			Data:       map[string]string{"versions": "4.19"},
		}
		c := fake.NewClientBuilder().WithScheme(sc).WithObjects(cm).Build()
		s := NewServer(c, "op-ns")

		got := s.getOCPChannels(context.Background())
		Expect(got).To(ContainElement(ocpChannelEntry{Name: "stable-4.19", Type: "ocp", Version: "4.19"}))
	})

	It("falls back to the built-in defaults when both GitHub and the ConfigMap are unavailable", func() {
		githubGraphDataChannelsURL = unreachableGitHubURL
		s := NewServerClusterWide(fake.NewClientBuilder().Build())

		got := s.getOCPChannels(context.Background())
		Expect(got).To(Equal(defaultOCPChannels))
	})

	It("serves the channel list as JSON via the HTTP handler", func() {
		s := &Server{}
		s.channelCache.entries = []ocpChannelEntry{{Name: "stable-4.20", Type: "ocp", Version: "4.20"}}
		s.channelCache.fetchedAt = time.Now()

		req := httptest.NewRequest("GET", "/api/v1/releases/channels", nil)
		rr := httptest.NewRecorder()
		s.handleGetOCPChannels(rr, req)

		Expect(rr.Code).To(Equal(http.StatusOK))
		Expect(rr.Body.String()).To(ContainSubstring("stable-4.20"))
	})
})
