package mirror

import (
	"context"
	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/state"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Collector", func() {
	var (
		col *Collector
		mc  *mirrorclient.MirrorClient
	)

	BeforeEach(func() {
		mc = mirrorclient.NewMirrorClient(nil, "")
		col = NewCollector(mc)
	})

	Context("CollectTargetImages", func() {
		It("should collect additional images correctly", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					AdditionalImages: []mirrorv1alpha1.AdditionalImage{
						{Name: "quay.io/custom/img:v1"},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "internal.registry.io",
				},
			}
			meta := &state.Metadata{MirroredImages: make(map[string]string)}

			results, err := col.CollectTargetImages(context.TODO(), spec, target, meta)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Source).To(Equal("quay.io/custom/img:v1"))
			Expect(results[0].Destination).To(Equal("internal.registry.io/quay.io/custom/img:v1"))
		})
	})

	Context("Type conversion", func() {
		It("should convert v1alpha1 to v2alpha1 correctly", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{
						{
							Catalog: "redhat-operators",
							IncludeConfig: mirrorv1alpha1.IncludeConfig{
								Packages: []mirrorv1alpha1.IncludePackage{
									{Name: "test-pkg"},
								},
							},
						},
					},
				},
			}
			_, _ = col.CollectTargetImages(context.TODO(), spec, &mirrorv1alpha1.MirrorTarget{}, nil)
		})
	})
})
