package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

var _ = Describe("ImageSet bundle selection validation", func() {
	newIS := func(name string, pkg mirrorv1alpha1.IncludePackage) *mirrorv1alpha1.ImageSet {
		return &mirrorv1alpha1.ImageSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{
				Operators: []mirrorv1alpha1.Operator{{
					Catalog:       "registry.example.com/catalog:v1",
					IncludeConfig: mirrorv1alpha1.IncludeConfig{Packages: []mirrorv1alpha1.IncludePackage{pkg}},
				}},
			}},
		}
	}
	bundles := []mirrorv1alpha1.SelectedBundle{{Name: "op.v1.0.0"}}

	It("accepts bundles on their own", func() {
		is := newIS("bundles-only", mirrorv1alpha1.IncludePackage{Name: "op", Bundles: bundles})
		Expect(k8sClient.Create(ctx, is)).To(Succeed())
		Expect(k8sClient.Delete(ctx, is)).To(Succeed())
	})

	DescribeTable("rejects bundles combined with other selectors",
		func(pkg mirrorv1alpha1.IncludePackage) {
			pkg.Name, pkg.Bundles = "op", bundles
			err := k8sClient.Create(ctx, newIS("bundles-mixed", pkg))
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "err = %v", err)
			Expect(err.Error()).To(ContainSubstring("bundles cannot be combined"))
		},
		Entry("channels", mirrorv1alpha1.IncludePackage{Channels: []mirrorv1alpha1.IncludeChannel{{Name: "stable"}}}),
		Entry("minVersion", mirrorv1alpha1.IncludePackage{IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "1.0.0"}}),
		Entry("maxVersion", mirrorv1alpha1.IncludePackage{IncludeBundle: mirrorv1alpha1.IncludeBundle{MaxVersion: "2.0.0"}}),
		Entry("previousVersions", mirrorv1alpha1.IncludePackage{PreviousVersions: 2}),
	)
})
