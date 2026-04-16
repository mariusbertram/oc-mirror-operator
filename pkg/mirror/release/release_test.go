package release

import (
	"context"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Release Resolver", func() {
	var (
		rr *ReleaseResolver
		mc *mirrorclient.MirrorClient
	)

	BeforeEach(func() {
		mc = mirrorclient.NewMirrorClient()
		rr = New(mc)
	})

	Context("ResolveRelease", func() {
		It("should return an error for non-existent versions", func() {
			images, err := rr.ResolveRelease(context.TODO(), "stable-4.15", "9.9.9", []string{"amd64"})
			Expect(err).To(HaveOccurred())
			Expect(images).To(BeNil())
		})
	})
})
