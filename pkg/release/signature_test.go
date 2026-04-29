package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/regclient/regclient"
)

var _ = Describe("SignatureClient", func() {

	Context("ErrNotImplemented", func() {
		It("should be a valid error", func() {
			Expect(ErrNotImplemented).To(HaveOccurred())
		})

		It("should have the correct message", func() {
			Expect(ErrNotImplemented.Error()).To(Equal("not implemented"))
		})
	})

	Context("NewSignatureClient", func() {
		It("should return a non-nil client when given nil", func() {
			client := NewSignatureClient(nil)
			Expect(client).ToNot(BeNil())
		})

		It("should return a non-nil client when given a real regclient", func() {
			rc := regclient.New()
			client := NewSignatureClient(rc)
			Expect(client).ToNot(BeNil())
		})
	})

	Context("ReplicateSignature", func() {
		It("should return ErrNotImplemented", func() {
			client := NewSignatureClient(nil)
			err := client.ReplicateSignature(context.TODO(), "src", "dst")
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, ErrNotImplemented)).To(BeTrue())
		})
	})

	Context("DownloadSignature", func() {
		var (
			originalBaseURL string
			srv             *httptest.Server
		)

		BeforeEach(func() {
			originalBaseURL = signatureBaseURL
		})

		AfterEach(func() {
			signatureBaseURL = originalBaseURL
			if srv != nil {
				srv.Close()
			}
		})

		It("returns signature bytes on HTTP 200", func() {
			fakeData := []byte("GPG-SIGNATURE-BYTES")
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Expect(r.URL.Path).To(ContainSubstring("sha256=abc123"))
				Expect(r.URL.Path).To(HaveSuffix("/signature-1"))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(fakeData)
			}))
			signatureBaseURL = srv.URL

			c := NewSignatureClient(nil)
			data, err := c.DownloadSignature(context.TODO(), "sha256:abc123")
			Expect(err).NotTo(HaveOccurred())
			Expect(data).To(Equal(fakeData))
		})

		It("returns error on HTTP 404", func() {
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			}))
			signatureBaseURL = srv.URL

			c := NewSignatureClient(nil)
			_, err := c.DownloadSignature(context.TODO(), "sha256:deadbeef")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("HTTP 404"))
		})

		It("returns error on HTTP 500", func() {
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}))
			signatureBaseURL = srv.URL

			c := NewSignatureClient(nil)
			_, err := c.DownloadSignature(context.TODO(), "sha256:abc")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("HTTP 500"))
		})

		It("returns error on empty response body", func() {
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			signatureBaseURL = srv.URL

			c := NewSignatureClient(nil)
			_, err := c.DownloadSignature(context.TODO(), "sha256:abc")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("empty signature body"))
		})

		It("converts digest colon to equals in URL path", func() {
			var capturedPath string
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("sig"))
			}))
			signatureBaseURL = srv.URL

			c := NewSignatureClient(nil)
			_, _ = c.DownloadSignature(context.TODO(), "sha256:abc123def456")
			Expect(capturedPath).To(Equal("/sha256=abc123def456/signature-1"))
		})

		It("returns error on cancelled context", func() {
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("sig"))
			}))
			signatureBaseURL = srv.URL

			ctx, cancel := context.WithCancel(context.TODO())
			cancel()

			c := NewSignatureClient(nil)
			_, err := c.DownloadSignature(ctx, "sha256:abc")
			Expect(err).To(HaveOccurred())
		})
	})
})
