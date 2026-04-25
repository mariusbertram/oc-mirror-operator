package release

import (
	"context"
	"errors"

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
})
