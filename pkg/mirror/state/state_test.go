package state

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("State", func() {
	Context("Metadata JSON round-trip", func() {
		It("serializes and deserializes correctly", func() {
			meta := &Metadata{
				MirroredImages: map[string]string{
					"registry.io/img:v1": "sha256:aaa",
					"registry.io/img:v2": "sha256:bbb",
				},
			}
			data, err := json.Marshal(meta)
			Expect(err).NotTo(HaveOccurred())

			var decoded Metadata
			Expect(json.Unmarshal(data, &decoded)).To(Succeed())
			Expect(decoded.MirroredImages).To(HaveLen(2))
			Expect(decoded.MirroredImages["registry.io/img:v1"]).To(Equal("sha256:aaa"))
		})

		It("handles empty MirroredImages", func() {
			meta := &Metadata{MirroredImages: map[string]string{}}
			data, err := json.Marshal(meta)
			Expect(err).NotTo(HaveOccurred())

			var decoded Metadata
			Expect(json.Unmarshal(data, &decoded)).To(Succeed())
			Expect(decoded.MirroredImages).To(BeEmpty())
		})

		It("handles nil MirroredImages", func() {
			meta := &Metadata{}
			data, err := json.Marshal(meta)
			Expect(err).NotTo(HaveOccurred())

			var decoded Metadata
			Expect(json.Unmarshal(data, &decoded)).To(Succeed())
			Expect(decoded.MirroredImages).To(BeNil())
		})
	})

	Context("Constants", func() {
		It("has expected media types", func() {
			Expect(MetadataConfigType).To(Equal("application/vnd.mirror.openshift.io.config.v1+json"))
			Expect(MetadataLayerType).To(Equal("application/vnd.mirror.openshift.io.metadata.v1+json"))
		})
	})
})
