package state

import (
	"context"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("State", func() {
	var (
		sm *StateManager
		mc *mirrorclient.MirrorClient
	)

	BeforeEach(func() {
		mc = mirrorclient.NewMirrorClient(nil, "")
		sm = New(mc)
	})

	Context("Metadata operations", func() {
		It("should write and read metadata (placeholder test)", func() {
			meta := &Metadata{
				MirroredImages: map[string]string{
					"registry.io/img:v1": "sha256:123",
				},
			}

			digest, err := sm.WriteMetadata(context.TODO(), "registry.io/meta", "latest", meta)
			Expect(err).NotTo(HaveOccurred())
			Expect(digest).To(Equal("sha256:dummy"))

			readMeta, _, err := sm.ReadMetadata(context.TODO(), "registry.io/meta", "latest")
			Expect(err).NotTo(HaveOccurred())
			Expect(readMeta).NotTo(BeNil())
		})
	})
})
