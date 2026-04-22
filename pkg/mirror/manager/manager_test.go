package manager

import (
	"context"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("Mirror Manager", func() {
	var (
		m      *MirrorManager
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		_ = mirrorv1alpha1.AddToScheme(scheme)
		_ = corev1.AddToScheme(scheme)

		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		cs := k8sfake.NewSimpleClientset()

		m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
	})

	Context("Reconcile Logic", func() {
		It("should handle empty image sets correctly", func() {
			err := m.reconcile(context.TODO())
			// This will fail because MirrorTarget is missing in the fake client
			Expect(err).To(HaveOccurred())
		})

		It("should work when MirrorTarget exists", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.io"},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(mt).Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
