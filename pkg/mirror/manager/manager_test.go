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

	Context("Operator cache versioning", func() {
		It("should build a versioned cache token", func() {
			token := operatorCacheValue("sha256:abc123")
			Expect(token).To(Equal(operatorCacheVersion + ":sha256:abc123"))
		})

		It("should match when version and digest are the same", func() {
			Expect(operatorCacheHit("v4:sha256:abc123", "sha256:abc123")).To(BeTrue())
		})

		It("should NOT match an old unversioned annotation (forces re-resolution on upgrade)", func() {
			Expect(operatorCacheHit("sha256:abc123", "sha256:abc123")).To(BeFalse())
		})

		It("should NOT match when digest changed", func() {
			Expect(operatorCacheHit("v4:sha256:old", "sha256:new")).To(BeFalse())
		})

		It("should NOT match empty cached value", func() {
			Expect(operatorCacheHit("", "sha256:abc123")).To(BeFalse())
		})
	})

	Context("setImageStateLocked", func() {
		It("should fall back to linear search when destToIS is empty", func() {
			m.imageStates["my-imageset"] = imagestate.ImageState{
				"reg.io/mirror/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  statePending,
				},
			}
			// destToIS is empty — fallback search should find the entry
			m.setImageStateLocked("reg.io/mirror/img:v1", stateMirrored, "")

			entry := m.imageStates["my-imageset"]["reg.io/mirror/img:v1"]
			Expect(entry.State).To(Equal(stateMirrored))
			Expect(m.destToIS["reg.io/mirror/img:v1"]).To(Equal("my-imageset"))
			Expect(m.dirtyStateNames["my-imageset"]).To(BeTrue())
		})

		It("should be idempotent for duplicate callbacks", func() {
			m.imageStates["my-imageset"] = imagestate.ImageState{
				"reg.io/mirror/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  statePending,
				},
			}
			m.destToIS["reg.io/mirror/img:v1"] = "my-imageset"

			// First failure: should increment RetryCount
			m.setImageStateLocked("reg.io/mirror/img:v1", stateFailed, "timeout")
			entry := m.imageStates["my-imageset"]["reg.io/mirror/img:v1"]
			Expect(entry.RetryCount).To(Equal(1))

			// Duplicate failure (HTTP retry): same state+error → no change
			m.setImageStateLocked("reg.io/mirror/img:v1", stateFailed, "timeout")
			Expect(entry.RetryCount).To(Equal(1))
		})

		It("should increment RetryCount for different errors", func() {
			m.imageStates["my-imageset"] = imagestate.ImageState{
				"reg.io/mirror/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  statePending,
				},
			}
			m.destToIS["reg.io/mirror/img:v1"] = "my-imageset"

			m.setImageStateLocked("reg.io/mirror/img:v1", stateFailed, "timeout")
			// Reset to Pending (as the reconcile loop does)
			m.imageStates["my-imageset"]["reg.io/mirror/img:v1"].State = statePending
			// New failure with different error
			m.setImageStateLocked("reg.io/mirror/img:v1", stateFailed, "connection refused")

			entry := m.imageStates["my-imageset"]["reg.io/mirror/img:v1"]
			Expect(entry.RetryCount).To(Equal(2))
		})

		It("should silently skip when entry does not exist", func() {
			// Should not panic
			m.setImageStateLocked("nonexistent", stateMirrored, "")
		})
	})

	Context("hasStaleCacheAnnotations", func() {
		It("should return false when no annotations exist", func() {
			is := &mirrorv1alpha1.ImageSet{}
			Expect(hasStaleCacheAnnotations(is)).To(BeFalse())
		})

		It("should return false when all catalog annotations have current version", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						mirrorv1alpha1.CatalogDigestAnnotationPrefix + "abc": operatorCacheVersion + ":sha256:xyz",
						"unrelated-annotation":                               "value",
					},
				},
			}
			Expect(hasStaleCacheAnnotations(is)).To(BeFalse())
		})

		It("should return true when a catalog annotation has an old version prefix", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						mirrorv1alpha1.CatalogDigestAnnotationPrefix + "abc": "v2:sha256:xyz",
					},
				},
			}
			Expect(hasStaleCacheAnnotations(is)).To(BeTrue())
		})

		It("should return true when a catalog annotation has no version prefix (legacy)", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						mirrorv1alpha1.CatalogDigestAnnotationPrefix + "abc": "sha256:xyz",
					},
				},
			}
			Expect(hasStaleCacheAnnotations(is)).To(BeTrue())
		})
	})

	Context("shouldResolve", func() {
		It("should return true when state is empty", func() {
			is := &mirrorv1alpha1.ImageSet{}
			mt := &mirrorv1alpha1.MirrorTarget{}
			Expect(shouldResolve(is, mt, nil)).To(BeTrue())
		})

		It("should return true when cache annotations have stale version", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						mirrorv1alpha1.CatalogDigestAnnotationPrefix + "abc": "v1:sha256:xyz",
					},
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			mt := &mirrorv1alpha1.MirrorTarget{}
			state := imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{State: "Pending"},
			}
			Expect(shouldResolve(is, mt, state)).To(BeTrue())
		})

		It("should return false when spec unchanged and cache is current", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						mirrorv1alpha1.CatalogDigestAnnotationPrefix + "abc": operatorCacheVersion + ":sha256:xyz",
					},
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			mt := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			state := imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{State: "Pending"},
			}
			Expect(shouldResolve(is, mt, state)).To(BeFalse())
		})
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
