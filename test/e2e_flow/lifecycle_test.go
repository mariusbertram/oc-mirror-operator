package e2e_flow

import (
	"context"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/internal/controller"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog/builder"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/manager"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Operator Lifecycle", func() {
	var (
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		_ = mirrorv1alpha1.AddToScheme(scheme)
		_ = corev1.AddToScheme(scheme)
		_ = metav1.AddMetaToScheme(scheme)
	})

	It("should process an ImageSet from creation to manager orchestration", func() {
		ctx := context.TODO()
		ns := "default"

		// 1. Setup MirrorTarget with ImageSet reference
		mt := &mirrorv1alpha1.MirrorTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "internal", Namespace: ns},
			Spec: mirrorv1alpha1.MirrorTargetSpec{
				Registry:  "registry.internal.io",
				ImageSets: []string{"app-sync"},
			},
		}

		// 2. Setup ImageSet
		is := &mirrorv1alpha1.ImageSet{
			ObjectMeta: metav1.ObjectMeta{Name: "app-sync", Namespace: ns},
			Spec: mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					AdditionalImages: []mirrorv1alpha1.AdditionalImage{
						{Name: "quay.io/my-app:v1"},
					},
				},
			},
		}

		cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(mt, is).WithStatusSubresource(is).Build()
		cs := k8sfake.NewSimpleClientset()

		// 3. Run ImageSet Reconciler
		mc := mirrorclient.NewMirrorClient(nil, "")
		_ = os.Setenv(builder.OperatorImageEnvVar, "test-operator:latest")
		bm, bmErr := builder.New()
		Expect(bmErr).NotTo(HaveOccurred())
		r := &controller.ImageSetReconciler{
			Client:          cl,
			Scheme:          scheme,
			MirrorClient:    mc,
			Collector:       mirror.NewCollector(mc),
			CatalogBuildMgr: bm,
		}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: is.Name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		// 4. Verify ImageSet status has been updated
		updatedIS := &mirrorv1alpha1.ImageSet{}
		_ = cl.Get(ctx, types.NamespacedName{Name: is.Name, Namespace: ns}, updatedIS)
		Expect(updatedIS.Status.ObservedGeneration).To(Equal(updatedIS.Generation))

		// 5. Run Mirror Manager
		m := manager.NewWithClients(cl, cs, mt.Name, ns, "test-image:latest", "", scheme)
		Expect(m).NotTo(BeNil())
	})
})
