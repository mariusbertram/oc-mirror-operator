package manager

import (
	"context"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("Manager Global Resources", func() {
	var (
		m      *MirrorManager
		scheme *runtime.Scheme
		ctx    context.Context
	)

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		_ = mirrorv1alpha1.AddToScheme(scheme)
		_ = corev1.AddToScheme(scheme)
		ctx = context.TODO()

		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		cs := k8sfake.NewSimpleClientset()

		m = NewWithClients(c, cs, "test-mt", "default", "test-image:latest", "", scheme)
	})

	It("should generate and save IDMS, ITMS and CatalogSource to ConfigMap", func() {
		mt := &mirrorv1alpha1.MirrorTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "test-mt", Namespace: "default"},
			Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.io"},
		}

		m.imageState["reg.io/mirror/img@sha256:123"] = &imagestate.ImageEntry{
			Source:    "quay.io/img@sha256:123",
			State:     "Mirrored",
			Origin:    imagestate.OriginOperator,
			OriginRef: "quay.io/catalog [pkg1]",
		}

		err := m.saveGlobalResources(ctx, mt)
		Expect(err).NotTo(HaveOccurred())

		cm := &corev1.ConfigMap{}
		err = m.Client.Get(ctx, types.NamespacedName{Name: "oc-mirror-test-mt-resources", Namespace: "default"}, cm)
		Expect(err).NotTo(HaveOccurred())

		Expect(cm.Data).To(HaveKey("idms.yaml"))
		Expect(cm.Data).To(HaveKey("itms.yaml"))
		Expect(cm.Data).To(HaveKey("catalogsource-catalog.yaml"))

		Expect(cm.Data["idms.yaml"]).To(ContainSubstring("ImageDigestMirrorSet"))
		Expect(cm.Data["catalogsource-catalog.yaml"]).To(ContainSubstring("CatalogSource"))
	})
})
