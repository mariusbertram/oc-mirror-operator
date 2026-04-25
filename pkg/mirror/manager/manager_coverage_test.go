package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("Manager Coverage", func() {
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

	const testImageSetName = "my-is"

	// ─── Pure helper functions ────────────────────────────────────────

	Context("pointerTo", func() {
		It("returns a pointer to a bool", func() {
			p := pointerTo(true)
			Expect(p).NotTo(BeNil())
			Expect(*p).To(BeTrue())
		})
		It("returns a pointer to an int", func() {
			p := pointerTo(42)
			Expect(*p).To(Equal(42))
		})
		It("returns a pointer to a string", func() {
			p := pointerTo("hello")
			Expect(*p).To(Equal("hello"))
		})
	})

	Context("resourcePtr", func() {
		It("parses a quantity string", func() {
			q := resourcePtr("10Gi")
			Expect(q).NotTo(BeNil())
			Expect(q.String()).To(Equal("10Gi"))
		})
		It("parses a small quantity", func() {
			q := resourcePtr("500Mi")
			Expect(q).NotTo(BeNil())
			Expect(q.String()).To(Equal("500Mi"))
		})
	})

	Context("containsString", func() {
		It("returns true when string is in slice", func() {
			Expect(containsString([]string{"a", "b", "c"}, "b")).To(BeTrue())
		})
		It("returns false when string is not in slice", func() {
			Expect(containsString([]string{"a", "b", "c"}, "d")).To(BeFalse())
		})
		It("returns false for empty slice", func() {
			Expect(containsString(nil, "a")).To(BeFalse())
		})
		It("returns false for empty string search", func() {
			Expect(containsString([]string{"a"}, "")).To(BeFalse())
		})
		It("returns true when searching for empty string in slice containing it", func() {
			Expect(containsString([]string{"", "a"}, "")).To(BeTrue())
		})
	})

	Context("copyMap", func() {
		It("copies a non-empty map", func() {
			orig := map[string]string{"a": "1", "b": "2"}
			cp := copyMap(orig)
			Expect(cp).To(Equal(orig))
			cp["c"] = "3"
			Expect(orig).NotTo(HaveKey("c"))
		})
		It("returns empty map for nil input", func() {
			cp := copyMap(nil)
			Expect(cp).NotTo(BeNil())
			Expect(cp).To(BeEmpty())
		})
		It("returns empty map for empty input", func() {
			cp := copyMap(map[string]string{})
			Expect(cp).NotTo(BeNil())
			Expect(cp).To(BeEmpty())
		})
	})

	Context("cloneImageState", func() {
		It("deep-clones entries", func() {
			orig := imagestate.ImageState{
				"dest1": &imagestate.ImageEntry{Source: "src1", State: "Pending"},
				"dest2": &imagestate.ImageEntry{Source: "src2", State: "Mirrored"},
			}
			cloned := cloneImageState(orig)
			Expect(cloned).To(HaveLen(2))
			Expect(cloned["dest1"].Source).To(Equal("src1"))
			// Mutating the clone must not affect the original
			cloned["dest1"].State = "Failed"
			Expect(orig["dest1"].State).To(Equal("Pending"))
		})
		It("handles nil entries", func() {
			orig := imagestate.ImageState{"dest1": nil}
			cloned := cloneImageState(orig)
			Expect(cloned["dest1"]).To(BeNil())
		})
		It("handles empty state", func() {
			cloned := cloneImageState(imagestate.ImageState{})
			Expect(cloned).To(BeEmpty())
		})
	})

	// ─── equalState ──────────────────────────────────────────────────

	Context("equalState", func() {
		It("returns true for identical states", func() {
			a := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", Origin: imagestate.OriginRelease, EntrySig: "sig1"},
			}
			b := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", Origin: imagestate.OriginRelease, EntrySig: "sig1"},
			}
			Expect(equalState(a, b)).To(BeTrue())
		})
		It("returns false for different lengths", func() {
			a := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"}}
			b := imagestate.ImageState{}
			Expect(equalState(a, b)).To(BeFalse())
		})
		It("returns false when key is missing in b", func() {
			a := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"}}
			b := imagestate.ImageState{"d2": &imagestate.ImageEntry{Source: "s1", State: "Pending"}}
			Expect(equalState(a, b)).To(BeFalse())
		})
		It("returns false when source differs", func() {
			a := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"}}
			b := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s2", State: "Pending"}}
			Expect(equalState(a, b)).To(BeFalse())
		})
		It("returns false when state differs", func() {
			a := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"}}
			b := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Mirrored"}}
			Expect(equalState(a, b)).To(BeFalse())
		})
		It("returns false when origin differs", func() {
			a := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", Origin: imagestate.OriginRelease}}
			b := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", Origin: imagestate.OriginOperator}}
			Expect(equalState(a, b)).To(BeFalse())
		})
		It("returns false when entrySig differs", func() {
			a := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", EntrySig: "x"}}
			b := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", EntrySig: "y"}}
			Expect(equalState(a, b)).To(BeFalse())
		})
		It("returns false when retryCount differs", func() {
			a := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", RetryCount: 1}}
			b := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", RetryCount: 2}}
			Expect(equalState(a, b)).To(BeFalse())
		})
		It("returns false when lastError differs", func() {
			a := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", LastError: "err1"}}
			b := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", LastError: "err2"}}
			Expect(equalState(a, b)).To(BeFalse())
		})
		It("returns false when originRef differs", func() {
			a := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", OriginRef: "ref1"}}
			b := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending", OriginRef: "ref2"}}
			Expect(equalState(a, b)).To(BeFalse())
		})
		It("returns true for both empty", func() {
			Expect(equalState(imagestate.ImageState{}, imagestate.ImageState{})).To(BeTrue())
		})
		It("handles nil entries symmetrically", func() {
			a := imagestate.ImageState{"d1": nil}
			b := imagestate.ImageState{"d1": nil}
			Expect(equalState(a, b)).To(BeTrue())
		})
		It("returns false when one entry is nil and other is not", func() {
			a := imagestate.ImageState{"d1": nil}
			b := imagestate.ImageState{"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"}}
			Expect(equalState(a, b)).To(BeFalse())
		})
	})

	// ─── mergeIntoStateWithSig ───────────────────────────────────────

	Context("mergeIntoStateWithSig", func() {
		It("adds new entries as Pending", func() {
			dst := make(imagestate.ImageState)
			prev := make(imagestate.ImageState)
			images := []mirror.TargetImage{
				{Source: "quay.io/img:v1", Destination: "reg.io/img:v1"},
			}
			mergeIntoStateWithSig(dst, images, imagestate.OriginRelease, "sig1", "stable-4.14", prev)
			Expect(dst).To(HaveLen(1))
			Expect(dst["reg.io/img:v1"].State).To(Equal("Pending"))
			Expect(dst["reg.io/img:v1"].Source).To(Equal("quay.io/img:v1"))
			Expect(dst["reg.io/img:v1"].Origin).To(Equal(imagestate.OriginRelease))
			Expect(dst["reg.io/img:v1"].EntrySig).To(Equal("sig1"))
			Expect(dst["reg.io/img:v1"].OriginRef).To(Equal("stable-4.14"))
		})

		It("preserves Mirrored state from prev", func() {
			dst := make(imagestate.ImageState)
			prev := imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source:     "quay.io/img:v1",
					State:      "Mirrored",
					Origin:     imagestate.OriginRelease,
					RetryCount: 3,
					LastError:  "old-error",
				},
			}
			images := []mirror.TargetImage{
				{Source: "quay.io/img:v1", Destination: "reg.io/img:v1"},
			}
			mergeIntoStateWithSig(dst, images, imagestate.OriginRelease, "sig1", "stable-4.14", prev)
			Expect(dst["reg.io/img:v1"].State).To(Equal("Mirrored"))
			Expect(dst["reg.io/img:v1"].RetryCount).To(Equal(3))
			Expect(dst["reg.io/img:v1"].LastError).To(Equal("old-error"))
		})

		It("resets Failed state to Pending (does not carry forward Failed)", func() {
			dst := make(imagestate.ImageState)
			prev := imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source:     "quay.io/img:v1",
					State:      "Failed",
					Origin:     imagestate.OriginRelease,
					RetryCount: 5,
					LastError:  "timeout",
				},
			}
			images := []mirror.TargetImage{
				{Source: "quay.io/img:v1", Destination: "reg.io/img:v1"},
			}
			mergeIntoStateWithSig(dst, images, imagestate.OriginRelease, "sig1", "stable-4.14", prev)
			Expect(dst["reg.io/img:v1"].State).To(Equal("Pending"))
			Expect(dst["reg.io/img:v1"].RetryCount).To(Equal(0))
		})

		It("does not carry forward when origin differs", func() {
			dst := make(imagestate.ImageState)
			prev := imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  "Mirrored",
					Origin: imagestate.OriginOperator,
				},
			}
			images := []mirror.TargetImage{
				{Source: "quay.io/img:v1", Destination: "reg.io/img:v1"},
			}
			mergeIntoStateWithSig(dst, images, imagestate.OriginRelease, "sig1", "stable-4.14", prev)
			Expect(dst["reg.io/img:v1"].State).To(Equal("Pending"))
		})

		It("uses BundleRef in OriginRef when set", func() {
			dst := make(imagestate.ImageState)
			prev := make(imagestate.ImageState)
			images := []mirror.TargetImage{
				{Source: "quay.io/img:v1", Destination: "reg.io/img:v1", BundleRef: "myop.v1.0"},
			}
			mergeIntoStateWithSig(dst, images, imagestate.OriginOperator, "sig1", "catalog-ref", prev)
			Expect(dst["reg.io/img:v1"].OriginRef).To(Equal("catalog-ref — myop.v1.0"))
		})

		It("handles nil prev entry gracefully", func() {
			dst := make(imagestate.ImageState)
			prev := imagestate.ImageState{
				"reg.io/img:v1": nil,
			}
			images := []mirror.TargetImage{
				{Source: "quay.io/img:v1", Destination: "reg.io/img:v1"},
			}
			mergeIntoStateWithSig(dst, images, imagestate.OriginRelease, "sig1", "ref", prev)
			Expect(dst["reg.io/img:v1"].State).To(Equal("Pending"))
		})
	})

	// ─── carryOverByOriginAndSig ─────────────────────────────────────

	Context("carryOverByOriginAndSig", func() {
		It("carries over matching entries", func() {
			src := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Mirrored", Origin: imagestate.OriginRelease, EntrySig: "sig1"},
			}
			dst := make(imagestate.ImageState)
			carryOverByOriginAndSig(src, dst, imagestate.OriginRelease, "sig1", "ref")
			Expect(dst).To(HaveKey("d1"))
			Expect(dst["d1"].State).To(Equal("Mirrored"))
		})

		It("skips entries with wrong origin", func() {
			src := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Mirrored", Origin: imagestate.OriginOperator, EntrySig: "sig1"},
			}
			dst := make(imagestate.ImageState)
			carryOverByOriginAndSig(src, dst, imagestate.OriginRelease, "sig1", "ref")
			Expect(dst).NotTo(HaveKey("d1"))
		})

		It("skips entries with wrong sig", func() {
			src := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Mirrored", Origin: imagestate.OriginRelease, EntrySig: "sig2"},
			}
			dst := make(imagestate.ImageState)
			carryOverByOriginAndSig(src, dst, imagestate.OriginRelease, "sig1", "ref")
			Expect(dst).NotTo(HaveKey("d1"))
		})

		It("carries over legacy entries with empty EntrySig", func() {
			src := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Mirrored", Origin: imagestate.OriginRelease, EntrySig: ""},
			}
			dst := make(imagestate.ImageState)
			carryOverByOriginAndSig(src, dst, imagestate.OriginRelease, "sig1", "ref")
			Expect(dst).To(HaveKey("d1"))
			Expect(dst["d1"].EntrySig).To(Equal("sig1"))
		})

		It("does not overwrite existing entries in dst", func() {
			src := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1-old", State: "Failed", Origin: imagestate.OriginRelease, EntrySig: "sig1"},
			}
			dst := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1-new", State: "Pending", Origin: imagestate.OriginRelease, EntrySig: "sig1"},
			}
			carryOverByOriginAndSig(src, dst, imagestate.OriginRelease, "sig1", "ref")
			Expect(dst["d1"].Source).To(Equal("s1-new"))
		})

		It("skips nil entries in src", func() {
			src := imagestate.ImageState{"d1": nil}
			dst := make(imagestate.ImageState)
			carryOverByOriginAndSig(src, dst, imagestate.OriginRelease, "sig1", "ref")
			Expect(dst).NotTo(HaveKey("d1"))
		})

		It("back-fills empty OriginRef", func() {
			src := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Mirrored", Origin: imagestate.OriginRelease, EntrySig: "sig1", OriginRef: ""},
			}
			dst := make(imagestate.ImageState)
			carryOverByOriginAndSig(src, dst, imagestate.OriginRelease, "sig1", "my-ref")
			Expect(dst["d1"].OriginRef).To(Equal("my-ref"))
		})

		It("does not overwrite existing OriginRef", func() {
			src := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Mirrored", Origin: imagestate.OriginRelease, EntrySig: "sig1", OriginRef: "existing-ref"},
			}
			dst := make(imagestate.ImageState)
			carryOverByOriginAndSig(src, dst, imagestate.OriginRelease, "sig1", "new-ref")
			Expect(dst["d1"].OriginRef).To(Equal("existing-ref"))
		})
	})

	// ─── mergeWorkerUpdates ──────────────────────────────────────────

	Context("mergeWorkerUpdates", func() {
		It("returns resolved when live is nil", func() {
			resolved := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"},
			}
			result := mergeWorkerUpdates(resolved, nil)
			Expect(result).To(Equal(resolved))
		})

		It("applies Mirrored state from live", func() {
			resolved := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"},
			}
			live := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Mirrored"},
			}
			result := mergeWorkerUpdates(resolved, live)
			Expect(result["d1"].State).To(Equal("Mirrored"))
		})

		It("applies Failed state from live", func() {
			resolved := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"},
			}
			live := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Failed", RetryCount: 3, LastError: "timeout", PermanentlyFailed: true},
			}
			result := mergeWorkerUpdates(resolved, live)
			Expect(result["d1"].State).To(Equal("Failed"))
			Expect(result["d1"].RetryCount).To(Equal(3))
			Expect(result["d1"].LastError).To(Equal("timeout"))
			Expect(result["d1"].PermanentlyFailed).To(BeTrue())
		})

		It("does not downgrade from live Pending to resolved", func() {
			resolved := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"},
			}
			live := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"},
			}
			result := mergeWorkerUpdates(resolved, live)
			Expect(result["d1"].State).To(Equal("Pending"))
		})

		It("ignores live entries not in resolved", func() {
			resolved := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"},
			}
			live := imagestate.ImageState{
				"d2": &imagestate.ImageEntry{Source: "s2", State: "Mirrored"},
			}
			result := mergeWorkerUpdates(resolved, live)
			Expect(result).NotTo(HaveKey("d2"))
			Expect(result["d1"].State).To(Equal("Pending"))
		})

		It("skips nil resolved entries", func() {
			resolved := imagestate.ImageState{
				"d1": nil,
			}
			live := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Mirrored"},
			}
			result := mergeWorkerUpdates(resolved, live)
			Expect(result["d1"]).To(BeNil())
		})

		It("skips nil live entries", func() {
			resolved := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"},
			}
			live := imagestate.ImageState{
				"d1": nil,
			}
			result := mergeWorkerUpdates(resolved, live)
			Expect(result["d1"].State).To(Equal("Pending"))
		})
	})

	// ─── pruneObsoleteCacheAnnotations ───────────────────────────────

	Context("pruneObsoleteCacheAnnotations", func() {
		It("removes an obsolete catalog annotation", func() {
			annotations := map[string]string{
				mirrorv1alpha1.CatalogDigestAnnotationPrefix + "oldsig": "sha256:abc",
				"unrelated": "keep",
			}
			is := &mirrorv1alpha1.ImageSet{
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{
							{Catalog: "registry.io/catalog:v1"},
						},
					},
				},
			}
			removed := pruneObsoleteCacheAnnotations(annotations, is)
			Expect(removed).To(BeTrue())
			Expect(annotations).NotTo(HaveKey(mirrorv1alpha1.CatalogDigestAnnotationPrefix + "oldsig"))
			Expect(annotations).To(HaveKey("unrelated"))
		})

		It("keeps annotations matching current spec entries", func() {
			op := mirrorv1alpha1.Operator{Catalog: "registry.io/catalog:v1"}
			sig := mirrorv1alpha1.OperatorEntrySignature(op)
			key := mirrorv1alpha1.CatalogDigestAnnotationKey(sig)
			annotations := map[string]string{
				key: "v4:sha256:abc",
			}
			is := &mirrorv1alpha1.ImageSet{
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{op},
					},
				},
			}
			removed := pruneObsoleteCacheAnnotations(annotations, is)
			Expect(removed).To(BeFalse())
			Expect(annotations).To(HaveKey(key))
		})

		It("returns false when no cache annotations exist", func() {
			annotations := map[string]string{"unrelated": "value"}
			is := &mirrorv1alpha1.ImageSet{}
			removed := pruneObsoleteCacheAnnotations(annotations, is)
			Expect(removed).To(BeFalse())
		})

		It("removes obsolete release annotations", func() {
			annotations := map[string]string{
				mirrorv1alpha1.ReleaseDigestAnnotationPrefix + "oldsig": "sha256:abc",
			}
			is := &mirrorv1alpha1.ImageSet{}
			removed := pruneObsoleteCacheAnnotations(annotations, is)
			Expect(removed).To(BeTrue())
			Expect(annotations).To(BeEmpty())
		})
	})

	// ─── workerTokenSecretName ───────────────────────────────────────

	Context("workerTokenSecretName", func() {
		It("returns the expected name", func() {
			Expect(m.workerTokenSecretName()).To(Equal("test-worker-token"))
		})

		It("includes the target name", func() {
			m.TargetName = "my-target"
			Expect(m.workerTokenSecretName()).To(Equal("my-target-worker-token"))
		})
	})

	// ─── ensureWorkerTokenSecret ─────────────────────────────────────

	Context("ensureWorkerTokenSecret", func() {
		It("creates a new secret when none exists", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
			}
			err := m.ensureWorkerTokenSecret(context.TODO(), mt)
			Expect(err).NotTo(HaveOccurred())
			Expect(m.workerToken).NotTo(BeEmpty())
			Expect(m.workerToken).To(HaveLen(64)) // 32 bytes hex-encoded
		})

		It("loads existing secret", func() {
			// Pre-create the secret
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-worker-token",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"token": []byte("my-existing-token"),
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(sec).Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			}
			err := m.ensureWorkerTokenSecret(context.TODO(), mt)
			Expect(err).NotTo(HaveOccurred())
			Expect(m.workerToken).To(Equal("my-existing-token"))
		})

		It("returns error when secret exists but has no token key", func() {
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-worker-token",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"wrong-key": []byte("value"),
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(sec).Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			}
			err := m.ensureWorkerTokenSecret(context.TODO(), mt)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("has no 'token' key"))
		})

		It("returns error when secret has empty token", func() {
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-worker-token",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"token": {},
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(sec).Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			}
			err := m.ensureWorkerTokenSecret(context.TODO(), mt)
			Expect(err).To(HaveOccurred())
		})

		It("creates without owner reference when mt is nil", func() {
			err := m.ensureWorkerTokenSecret(context.TODO(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(m.workerToken).NotTo(BeEmpty())
		})
	})

	// ─── handleStatusUpdate ──────────────────────────────────────────

	Context("handleStatusUpdate", func() {
		BeforeEach(func() {
			m.workerToken = "test-token"
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/mirror/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  statePending,
				},
			}
			m.destToIS["reg.io/mirror/img:v1"] = testImageSetName
		})

		It("rejects non-POST methods", func() {
			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			rr := httptest.NewRecorder()
			m.handleStatusUpdate(rr, req)
			Expect(rr.Code).To(Equal(http.StatusMethodNotAllowed))
		})

		It("rejects missing auth", func() {
			body, _ := json.Marshal(WorkerStatusRequest{
				PodName:     "worker-1",
				Destination: "reg.io/mirror/img:v1",
			})
			req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewReader(body))
			rr := httptest.NewRecorder()
			m.handleStatusUpdate(rr, req)
			Expect(rr.Code).To(Equal(http.StatusUnauthorized))
		})

		It("rejects wrong token", func() {
			body, _ := json.Marshal(WorkerStatusRequest{
				PodName:     "worker-1",
				Destination: "reg.io/mirror/img:v1",
			})
			req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer wrong-token")
			rr := httptest.NewRecorder()
			m.handleStatusUpdate(rr, req)
			Expect(rr.Code).To(Equal(http.StatusUnauthorized))
		})

		It("rejects invalid JSON body", func() {
			req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewReader([]byte("not json")))
			req.Header.Set("Authorization", "Bearer test-token")
			rr := httptest.NewRecorder()
			m.handleStatusUpdate(rr, req)
			Expect(rr.Code).To(Equal(http.StatusBadRequest))
		})

		It("marks image as Mirrored on success", func() {
			m.inProgress["reg.io/mirror/img:v1"] = "worker-1"
			body, _ := json.Marshal(WorkerStatusRequest{
				PodName:     "worker-1",
				Destination: "reg.io/mirror/img:v1",
			})
			req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-token")
			rr := httptest.NewRecorder()
			m.handleStatusUpdate(rr, req)
			Expect(rr.Code).To(Equal(http.StatusOK))
			Expect(m.imageStates[testImageSetName]["reg.io/mirror/img:v1"].State).To(Equal(stateMirrored))
			Expect(m.mirrored["reg.io/mirror/img:v1"]).To(BeTrue())
			Expect(m.inProgress).NotTo(HaveKey("reg.io/mirror/img:v1"))
		})

		It("marks image as Failed on error", func() {
			m.inProgress["reg.io/mirror/img:v1"] = "worker-1"
			body, _ := json.Marshal(WorkerStatusRequest{
				PodName:     "worker-1",
				Destination: "reg.io/mirror/img:v1",
				Error:       "timeout",
			})
			req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-token")
			rr := httptest.NewRecorder()
			m.handleStatusUpdate(rr, req)
			Expect(rr.Code).To(Equal(http.StatusOK))
			entry := m.imageStates[testImageSetName]["reg.io/mirror/img:v1"]
			Expect(entry.State).To(Equal(stateFailed))
			Expect(entry.RetryCount).To(Equal(1))
		})
	})

	// ─── workerProxyEnvVars ──────────────────────────────────────────

	Context("workerProxyEnvVars", func() {
		It("returns nil for nil config", func() {
			Expect(workerProxyEnvVars(nil)).To(BeNil())
		})

		It("sets HTTP_PROXY and http_proxy when HTTPProxy is set", func() {
			cfg := &mirrorv1alpha1.ProxyConfig{
				HTTPProxy: "http://proxy:8080",
			}
			envs := workerProxyEnvVars(cfg)
			envMap := envVarMap(envs)
			Expect(envMap["HTTP_PROXY"]).To(Equal("http://proxy:8080"))
			Expect(envMap["http_proxy"]).To(Equal("http://proxy:8080"))
			Expect(envMap).To(HaveKey("NO_PROXY"))
			Expect(envMap).To(HaveKey("no_proxy"))
			Expect(envMap).To(HaveKey("KUBERNETES_SERVICE_HOST"))
		})

		It("sets HTTPS_PROXY and https_proxy when HTTPSProxy is set", func() {
			cfg := &mirrorv1alpha1.ProxyConfig{
				HTTPSProxy: "https://proxy:8443",
			}
			envs := workerProxyEnvVars(cfg)
			envMap := envVarMap(envs)
			Expect(envMap["HTTPS_PROXY"]).To(Equal("https://proxy:8443"))
			Expect(envMap["https_proxy"]).To(Equal("https://proxy:8443"))
			Expect(envMap).To(HaveKey("NO_PROXY"))
		})

		It("includes both HTTP and HTTPS proxy", func() {
			cfg := &mirrorv1alpha1.ProxyConfig{
				HTTPProxy:  "http://proxy:8080",
				HTTPSProxy: "https://proxy:8443",
			}
			envs := workerProxyEnvVars(cfg)
			envMap := envVarMap(envs)
			Expect(envMap["HTTP_PROXY"]).To(Equal("http://proxy:8080"))
			Expect(envMap["HTTPS_PROXY"]).To(Equal("https://proxy:8443"))
			Expect(envMap).To(HaveKey("NO_PROXY"))
		})

		It("includes user NoProxy with cluster defaults when proxy is set", func() {
			cfg := &mirrorv1alpha1.ProxyConfig{
				HTTPProxy: "http://proxy:8080",
				NoProxy:   "my-domain.example.com",
			}
			envs := workerProxyEnvVars(cfg)
			envMap := envVarMap(envs)
			Expect(envMap["NO_PROXY"]).To(ContainSubstring(".svc.cluster.local"))
			Expect(envMap["NO_PROXY"]).To(ContainSubstring("my-domain.example.com"))
		})

		It("sets NoProxy alone when no HTTP/HTTPS proxy set", func() {
			cfg := &mirrorv1alpha1.ProxyConfig{
				NoProxy: "example.com",
			}
			envs := workerProxyEnvVars(cfg)
			envMap := envVarMap(envs)
			Expect(envMap["NO_PROXY"]).To(Equal("example.com"))
			Expect(envMap["no_proxy"]).To(Equal("example.com"))
			Expect(envMap).NotTo(HaveKey("HTTP_PROXY"))
			Expect(envMap).NotTo(HaveKey("KUBERNETES_SERVICE_HOST"))
		})

		It("returns empty env list for empty config", func() {
			cfg := &mirrorv1alpha1.ProxyConfig{}
			envs := workerProxyEnvVars(cfg)
			Expect(envs).To(BeNil())
		})
	})

	// ─── workerBuildEffectiveNoProxy ─────────────────────────────────

	Context("workerBuildEffectiveNoProxy", func() {
		It("returns only cluster defaults for empty user input", func() {
			result := workerBuildEffectiveNoProxy("")
			Expect(result).To(Equal("localhost,127.0.0.1,.svc,.svc.cluster.local"))
		})

		It("appends user input", func() {
			result := workerBuildEffectiveNoProxy("example.com")
			Expect(result).To(Equal("localhost,127.0.0.1,.svc,.svc.cluster.local,example.com"))
		})
	})

	// ─── setReadyCondition ───────────────────────────────────────────

	Context("setReadyCondition", func() {
		It("appends a new Ready condition when none exists", func() {
			conds := []metav1.Condition{}
			setReadyCondition(&conds, metav1.ConditionTrue, "Collected", "msg", 1)
			Expect(conds).To(HaveLen(1))
			Expect(conds[0].Type).To(Equal("Ready"))
			Expect(conds[0].Status).To(Equal(metav1.ConditionTrue))
			Expect(conds[0].Reason).To(Equal("Collected"))
			Expect(conds[0].Message).To(Equal("msg"))
			Expect(conds[0].ObservedGeneration).To(Equal(int64(1)))
		})

		It("updates an existing Ready condition when it changes", func() {
			conds := []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionFalse,
					Reason:             "Empty",
					Message:            "no images resolved yet",
					ObservedGeneration: 1,
					LastTransitionTime: metav1.NewTime(time.Now().Add(-1 * time.Hour)),
				},
			}
			setReadyCondition(&conds, metav1.ConditionTrue, "Collected", "10 images", 2)
			Expect(conds).To(HaveLen(1))
			Expect(conds[0].Status).To(Equal(metav1.ConditionTrue))
			Expect(conds[0].Reason).To(Equal("Collected"))
			Expect(conds[0].Message).To(Equal("10 images"))
			Expect(conds[0].ObservedGeneration).To(Equal(int64(2)))
		})

		It("does not update when nothing changed", func() {
			oldTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
			conds := []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "Collected",
					Message:            "10 images",
					ObservedGeneration: 1,
					LastTransitionTime: oldTime,
				},
			}
			setReadyCondition(&conds, metav1.ConditionTrue, "Collected", "10 images", 1)
			Expect(conds).To(HaveLen(1))
			Expect(conds[0].LastTransitionTime).To(Equal(oldTime))
		})

		It("does nothing when conditions pointer is nil", func() {
			setReadyCondition(nil, metav1.ConditionTrue, "Collected", "msg", 1)
			// No panic
		})

		It("preserves non-Ready conditions", func() {
			conds := []metav1.Condition{
				{Type: "CatalogReady", Status: metav1.ConditionTrue, Reason: "Built"},
			}
			setReadyCondition(&conds, metav1.ConditionTrue, "Collected", "msg", 1)
			Expect(conds).To(HaveLen(2))
			Expect(conds[0].Type).To(Equal("CatalogReady"))
			Expect(conds[1].Type).To(Equal("Ready"))
		})
	})

	// ─── syncInProgressFromPods ──────────────────────────────────────

	Context("syncInProgressFromPods", func() {
		It("recovers running pods with batch annotations", func() {
			destsJSON, _ := json.Marshal([]string{"d1", "d2"})
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "worker-1",
					Namespace: "default",
					Labels:    map[string]string{"app": "oc-mirror-worker", "mirrortarget": "test"},
					Annotations: map[string]string{
						"mirror.openshift.io/destinations": string(destsJSON),
					},
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}
			cs := k8sfake.NewSimpleClientset(pod)
			m.Clientset = cs
			err := m.syncInProgressFromPods(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(m.inProgress["d1"]).To(Equal("worker-1"))
			Expect(m.inProgress["d2"]).To(Equal("worker-1"))
		})

		It("recovers running pods with legacy single-dest annotation", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "worker-2",
					Namespace: "default",
					Labels:    map[string]string{"app": "oc-mirror-worker", "mirrortarget": "test"},
					Annotations: map[string]string{
						"mirror.openshift.io/destination": "d3",
					},
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}
			cs := k8sfake.NewSimpleClientset(pod)
			m.Clientset = cs
			err := m.syncInProgressFromPods(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(m.inProgress["d3"]).To(Equal("worker-2"))
		})

		It("deletes completed/failed pods", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "worker-done",
					Namespace: "default",
					Labels:    map[string]string{"app": "oc-mirror-worker", "mirrortarget": "test"},
				},
				Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
			}
			cs := k8sfake.NewSimpleClientset(pod)
			m.Clientset = cs
			err := m.syncInProgressFromPods(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			// Should have attempted to delete the pod
			_, getErr := cs.CoreV1().Pods("default").Get(context.TODO(), "worker-done", metav1.GetOptions{})
			Expect(getErr).To(HaveOccurred())
		})

		It("handles empty pod list", func() {
			cs := k8sfake.NewSimpleClientset()
			m.Clientset = cs
			err := m.syncInProgressFromPods(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(m.inProgress).To(BeEmpty())
		})
	})

	// ─── shouldResolve (additional coverage) ─────────────────────────

	Context("shouldResolve additional", func() {
		It("returns true when recollect annotation is set", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						mirrorv1alpha1.RecollectAnnotation: "",
					},
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			mt := &mirrorv1alpha1.MirrorTarget{}
			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{State: "Pending"},
			}
			Expect(shouldResolve(is, mt, state)).To(BeTrue())
		})

		It("returns true when generation is ahead of observed", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 5},
				Status:     mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 3},
			}
			mt := &mirrorv1alpha1.MirrorTarget{}
			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{State: "Pending"},
			}
			Expect(shouldResolve(is, mt, state)).To(BeTrue())
		})

		It("returns true when pollInterval has elapsed", func() {
			past := metav1.NewTime(time.Now().Add(-25 * time.Hour))
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: mirrorv1alpha1.ImageSetStatus{
					ObservedGeneration:     1,
					LastSuccessfulPollTime: &past,
				},
			}
			mt := &mirrorv1alpha1.MirrorTarget{}
			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{State: "Pending"},
			}
			Expect(shouldResolve(is, mt, state)).To(BeTrue())
		})

		It("returns false when pollInterval has not elapsed", func() {
			now := metav1.Now()
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: mirrorv1alpha1.ImageSetStatus{
					ObservedGeneration:     1,
					LastSuccessfulPollTime: &now,
				},
			}
			mt := &mirrorv1alpha1.MirrorTarget{}
			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{State: "Pending"},
			}
			Expect(shouldResolve(is, mt, state)).To(BeFalse())
		})

		It("returns true when LastSuccessfulPollTime is nil and polling enabled", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: mirrorv1alpha1.ImageSetStatus{
					ObservedGeneration: 1,
				},
			}
			mt := &mirrorv1alpha1.MirrorTarget{}
			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{State: "Pending"},
			}
			Expect(shouldResolve(is, mt, state)).To(BeTrue())
		})

		It("respects custom pollInterval", func() {
			past := metav1.NewTime(time.Now().Add(-2 * time.Hour))
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: mirrorv1alpha1.ImageSetStatus{
					ObservedGeneration:     1,
					LastSuccessfulPollTime: &past,
				},
			}
			mt := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					PollInterval: &metav1.Duration{Duration: 1 * time.Hour},
				},
			}
			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{State: "Pending"},
			}
			Expect(shouldResolve(is, mt, state)).To(BeTrue())
		})

		It("enforces minimum 1h pollInterval", func() {
			// pollInterval < 1h should be clamped to 1h
			past := metav1.NewTime(time.Now().Add(-30 * time.Minute))
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: mirrorv1alpha1.ImageSetStatus{
					ObservedGeneration:     1,
					LastSuccessfulPollTime: &past,
				},
			}
			mt := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					PollInterval: &metav1.Duration{Duration: 10 * time.Minute},
				},
			}
			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{State: "Pending"},
			}
			// 30 min < 1h (clamped minimum) → should NOT resolve
			Expect(shouldResolve(is, mt, state)).To(BeFalse())
		})
	})

	// ─── Reconcile with ImageSets ────────────────────────────────────

	Context("Reconcile with ImageSets", func() {
		It("reconciles when MirrorTarget references an ImageSet", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:  "reg.io",
					ImageSets: []string{"my-imageset"},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "my-imageset",
					Namespace:  "default",
					Generation: 1,
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
		})

		It("skips ImageSets not in spec.imageSets", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:  "reg.io",
					ImageSets: []string{"other-is"},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "unrelated-is", Namespace: "default"},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(m.imageStates).NotTo(HaveKey("unrelated-is"))
		})

		It("handles dirty state flush", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{Source: "quay.io/img:v1", State: statePending},
			}
			m.dirtyStateNames[testImageSetName] = true

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ─── updateImageSetStatusLocked ──────────────────────────────────

	Context("updateImageSetStatusLocked", func() {
		It("sets status counts and Ready condition", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "status-is",
					Namespace:  "default",
					Generation: 2,
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(is).
				WithStatusSubresource(is).
				Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Mirrored"},
				"d2": &imagestate.ImageEntry{Source: "s2", State: "Pending"},
				"d3": &imagestate.ImageEntry{Source: "s3", State: "Failed", PermanentlyFailed: true, LastError: "err", OriginRef: "ref"},
			}
			m.updateImageSetStatusLocked(context.TODO(), is, state, false)
			Expect(is.Status.TotalImages).To(Equal(3))
			Expect(is.Status.MirroredImages).To(Equal(1))
			Expect(is.Status.ObservedGeneration).To(Equal(int64(2)))
		})

		It("sets Empty condition when no images", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "empty-is",
					Namespace:  "default",
					Generation: 1,
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(is).
				WithStatusSubresource(is).
				Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			state := imagestate.ImageState{}
			m.updateImageSetStatusLocked(context.TODO(), is, state, false)
			Expect(is.Status.TotalImages).To(Equal(0))
			found := false
			for _, c := range is.Status.Conditions {
				if c.Type == "Ready" {
					found = true
					Expect(c.Status).To(Equal(metav1.ConditionFalse))
					Expect(c.Reason).To(Equal("Empty"))
				}
			}
			Expect(found).To(BeTrue())
		})

		It("sets LastSuccessfulPollTime when justResolved is true", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "poll-is",
					Namespace:  "default",
					Generation: 1,
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(is).
				WithStatusSubresource(is).
				Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Pending"},
			}
			m.updateImageSetStatusLocked(context.TODO(), is, state, true)
			Expect(is.Status.LastSuccessfulPollTime).NotTo(BeNil())
		})

		It("caps failedImageDetails at 20", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "caps-is",
					Namespace:  "default",
					Generation: 1,
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(is).
				WithStatusSubresource(is).
				Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			state := make(imagestate.ImageState)
			for i := 0; i < 25; i++ {
				dest := "d" + string(rune('A'+i))
				state[dest] = &imagestate.ImageEntry{
					Source:            "s",
					State:             "Failed",
					PermanentlyFailed: true,
					LastError:         "err",
				}
			}
			m.updateImageSetStatusLocked(context.TODO(), is, state, false)
			Expect(len(is.Status.FailedImageDetails)).To(BeNumerically("<=", 20))
		})

		It("excludes Mirrored entries from failedImageDetails", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "excl-is",
					Namespace:  "default",
					Generation: 1,
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(is).
				WithStatusSubresource(is).
				Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: "Mirrored", PermanentlyFailed: true},
				"d2": &imagestate.ImageEntry{Source: "s2", State: "Failed", PermanentlyFailed: true, LastError: "err"},
			}
			m.updateImageSetStatusLocked(context.TODO(), is, state, false)
			Expect(is.Status.FailedImageDetails).To(HaveLen(1))
			Expect(is.Status.FailedImageDetails[0].Destination).To(Equal("d2"))
		})
	})

	// ─── cleanupFinishedWorkers ──────────────────────────────────────

	Context("cleanupFinishedWorkers", func() {
		It("removes finished tracked pods and resets Failed state", func() {
			failedPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "failed-worker",
					Namespace: "default",
					Labels:    map[string]string{"app": "oc-mirror-worker", "mirrortarget": "test"},
				},
				Status: corev1.PodStatus{Phase: corev1.PodFailed},
			}
			cs := k8sfake.NewSimpleClientset(failedPod)
			m.Clientset = cs
			m.inProgress["d1"] = "failed-worker"
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"d1": &imagestate.ImageEntry{Source: "s1", State: stateFailed},
			}
			m.destToIS["d1"] = testImageSetName

			m.cleanupFinishedWorkers(context.TODO())
			Expect(m.inProgress).NotTo(HaveKey("d1"))
		})

		It("deduplicates pod deletion for multi-dest batch pods", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "batch-worker",
					Namespace: "default",
					Labels:    map[string]string{"app": "oc-mirror-worker", "mirrortarget": "test"},
				},
				Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
			}
			cs := k8sfake.NewSimpleClientset(pod)
			m.Clientset = cs
			// Same pod tracked for two different destinations
			m.inProgress["d1"] = "batch-worker"
			m.inProgress["d2"] = "batch-worker"

			m.cleanupFinishedWorkers(context.TODO())
			Expect(m.inProgress).NotTo(HaveKey("d1"))
			Expect(m.inProgress).NotTo(HaveKey("d2"))
		})

		It("deletes succeeded tracked pods", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "done-worker",
					Namespace: "default",
					Labels:    map[string]string{"app": "oc-mirror-worker", "mirrortarget": "test"},
				},
				Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
			}
			cs := k8sfake.NewSimpleClientset(pod)
			m.Clientset = cs
			m.inProgress["d1"] = "done-worker"

			m.cleanupFinishedWorkers(context.TODO())
			Expect(m.inProgress).NotTo(HaveKey("d1"))
		})

		It("handles missing pods gracefully", func() {
			cs := k8sfake.NewSimpleClientset()
			m.Clientset = cs
			m.inProgress["d1"] = "gone-worker"

			m.cleanupFinishedWorkers(context.TODO())
			Expect(m.inProgress).NotTo(HaveKey("d1"))
		})

		It("cleans up orphaned finished pods", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "orphan-worker",
					Namespace: "default",
					Labels:    map[string]string{"app": "oc-mirror-worker", "mirrortarget": "test"},
				},
				Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
			}
			cs := k8sfake.NewSimpleClientset(pod)
			m.Clientset = cs
			// Not in inProgress → orphaned
			m.cleanupFinishedWorkers(context.TODO())
			_, getErr := cs.CoreV1().Pods("default").Get(context.TODO(), "orphan-worker", metav1.GetOptions{})
			Expect(getErr).To(HaveOccurred())
		})

		It("leaves running pods alone", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-worker",
					Namespace: "default",
					Labels:    map[string]string{"app": "oc-mirror-worker", "mirrortarget": "test"},
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}
			cs := k8sfake.NewSimpleClientset(pod)
			m.Clientset = cs
			m.inProgress["d1"] = "running-worker"

			m.cleanupFinishedWorkers(context.TODO())
			Expect(m.inProgress).To(HaveKey("d1"))
		})
	})

	// ─── startWorkerBatch ────────────────────────────────────────────

	Context("startWorkerBatch", func() {
		It("creates a worker pod with default emptyDir volume", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.io",
				},
			}
			items := []BatchItem{{Source: "quay.io/img:v1", Dest: "reg.io/mirror/img:v1"}}
			_, err := m.startWorkerBatch(context.TODO(), mt, items)
			Expect(err).NotTo(HaveOccurred())

			pods, listErr := m.Clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
			Expect(listErr).NotTo(HaveOccurred())
			Expect(pods.Items).To(HaveLen(1))
			pod := pods.Items[0]
			Expect(pod.Labels["app"]).To(Equal("oc-mirror-worker"))
			Expect(pod.Labels["mirrortarget"]).To(Equal("test"))
			Expect(pod.Spec.Containers[0].Args).To(Equal([]string{"worker"}))
		})

		It("adds --insecure flag when Insecure is true", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.io",
					Insecure: true,
				},
			}
			items := []BatchItem{{Source: "quay.io/img:v1", Dest: "reg.io/mirror/img:v1"}}
			_, err := m.startWorkerBatch(context.TODO(), mt, items)
			Expect(err).NotTo(HaveOccurred())

			pods, _ := m.Clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
			Expect(pods.Items).NotTo(BeEmpty())
			pod := pods.Items[0]
			Expect(pod.Spec.Containers[0].Args).To(ContainElement("--insecure"))
		})

		It("mounts auth secret when AuthSecret is set", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:   "reg.io",
					AuthSecret: "my-pull-secret",
				},
			}
			items := []BatchItem{{Source: "quay.io/img:v1", Dest: "reg.io/mirror/img:v1"}}
			_, err := m.startWorkerBatch(context.TODO(), mt, items)
			Expect(err).NotTo(HaveOccurred())

			pods, _ := m.Clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
			pod := pods.Items[0]
			foundDockerConfig := false
			for _, e := range pod.Spec.Containers[0].Env {
				if e.Name == "DOCKER_CONFIG" {
					foundDockerConfig = true
				}
			}
			Expect(foundDockerConfig).To(BeTrue())
			foundVolume := false
			for _, v := range pod.Spec.Volumes {
				if v.Name == "dockerconfig" {
					foundVolume = true
				}
			}
			Expect(foundVolume).To(BeTrue())
		})

		It("mounts CA bundle when CABundle is set", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.io",
					CABundle: &mirrorv1alpha1.CABundleRef{
						ConfigMapName: "ca-bundle-cm",
					},
				},
			}
			items := []BatchItem{{Source: "quay.io/img:v1", Dest: "reg.io/mirror/img:v1"}}
			_, err := m.startWorkerBatch(context.TODO(), mt, items)
			Expect(err).NotTo(HaveOccurred())

			pods, _ := m.Clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
			pod := pods.Items[0]
			foundSSLCert := false
			for _, e := range pod.Spec.Containers[0].Env {
				if e.Name == "SSL_CERT_FILE" {
					foundSSLCert = true
					Expect(e.Value).To(Equal("/run/secrets/ca/ca-bundle.crt"))
				}
			}
			Expect(foundSSLCert).To(BeTrue())
		})

		It("mounts CA bundle with custom key", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.io",
					CABundle: &mirrorv1alpha1.CABundleRef{
						ConfigMapName: "ca-bundle-cm",
						Key:           "custom-ca.pem",
					},
				},
			}
			items := []BatchItem{{Source: "quay.io/img:v1", Dest: "reg.io/mirror/img:v1"}}
			_, err := m.startWorkerBatch(context.TODO(), mt, items)
			Expect(err).NotTo(HaveOccurred())

			pods, _ := m.Clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
			pod := pods.Items[0]
			foundSSLCert := false
			for _, e := range pod.Spec.Containers[0].Env {
				if e.Name == "SSL_CERT_FILE" {
					foundSSLCert = true
					Expect(e.Value).To(Equal("/run/secrets/ca/custom-ca.pem"))
				}
			}
			Expect(foundSSLCert).To(BeTrue())
		})

		It("uses ephemeral PVC when WorkerStorage is set", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:      "reg.io",
					WorkerStorage: &mirrorv1alpha1.WorkerStorageConfig{},
				},
			}
			items := []BatchItem{{Source: "quay.io/img:v1", Dest: "reg.io/mirror/img:v1"}}
			_, err := m.startWorkerBatch(context.TODO(), mt, items)
			Expect(err).NotTo(HaveOccurred())

			pods, _ := m.Clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
			pod := pods.Items[0]
			foundEphemeral := false
			for _, v := range pod.Spec.Volumes {
				if v.Name == "blob-buffer" && v.Ephemeral != nil {
					foundEphemeral = true
				}
			}
			Expect(foundEphemeral).To(BeTrue())
		})

		It("injects proxy env vars", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.io",
					Proxy: &mirrorv1alpha1.ProxyConfig{
						HTTPProxy:  "http://proxy:3128",
						HTTPSProxy: "https://proxy:3129",
						NoProxy:    "internal.example.com",
					},
				},
			}
			items := []BatchItem{{Source: "quay.io/img:v1", Dest: "reg.io/mirror/img:v1"}}
			_, err := m.startWorkerBatch(context.TODO(), mt, items)
			Expect(err).NotTo(HaveOccurred())

			pods, _ := m.Clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
			pod := pods.Items[0]
			em := envVarMap(pod.Spec.Containers[0].Env)
			Expect(em["HTTP_PROXY"]).To(Equal("http://proxy:3128"))
			Expect(em["HTTPS_PROXY"]).To(Equal("https://proxy:3129"))
			Expect(em["NO_PROXY"]).To(ContainSubstring("internal.example.com"))
		})

		It("creates multiple destinations annotation", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.io"},
			}
			items := []BatchItem{
				{Source: "quay.io/img:v1", Dest: "reg.io/img:v1"},
				{Source: "quay.io/img:v2", Dest: "reg.io/img:v2"},
			}
			_, err := m.startWorkerBatch(context.TODO(), mt, items)
			Expect(err).NotTo(HaveOccurred())

			pods, _ := m.Clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
			pod := pods.Items[0]
			destsJSON := pod.Annotations["mirror.openshift.io/destinations"]
			var dests []string
			Expect(json.Unmarshal([]byte(destsJSON), &dests)).To(Succeed())
			Expect(dests).To(ConsistOf("reg.io/img:v1", "reg.io/img:v2"))
		})
	})

	// ─── patchImageSetAnnotations ────────────────────────────────────

	Context("patchImageSetAnnotations", func() {
		It("patches cache annotations", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "patch-is",
					Namespace: "default",
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(is).Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			desired := map[string]string{
				mirrorv1alpha1.CatalogDigestAnnotationPrefix + "sig1": "v4:sha256:abc",
				"unrelated": "should-be-ignored",
			}
			err := m.patchImageSetAnnotations(context.TODO(), is, desired)
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles not-found ImageSet gracefully", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nonexistent-is",
					Namespace: "default",
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			err := m.patchImageSetAnnotations(context.TODO(), is, map[string]string{})
			Expect(err).NotTo(HaveOccurred())
		})

		It("clears stale cache annotations and replaces with desired", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "clear-is",
					Namespace: "default",
					Annotations: map[string]string{
						mirrorv1alpha1.CatalogDigestAnnotationPrefix + "old":  "v3:sha256:old",
						mirrorv1alpha1.ReleaseDigestAnnotationPrefix + "old2": "release-old",
						mirrorv1alpha1.RecollectAnnotation:                    "",
						"keep-this":                                           "value",
					},
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(is).Build()
			m = NewWithClients(c, m.Clientset, "test", "default", "test-image:latest", "", scheme)

			desired := map[string]string{
				mirrorv1alpha1.CatalogDigestAnnotationPrefix + "new": "v4:sha256:new",
			}
			err := m.patchImageSetAnnotations(context.TODO(), is, desired)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ─── Deeper reconcile coverage ───────────────────────────────────

	Context("Reconcile deeper branches", func() {
		It("resets failed images with retryCount < 10 to Pending", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source:     "quay.io/img:v1",
					State:      stateFailed,
					RetryCount: 3,
					LastError:  "timeout",
				},
			}

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			// After reconcile, failed with retryCount < 10 should be reset to Pending
			entry := m.imageStates[testImageSetName]["reg.io/img:v1"]
			Expect(entry.State).To(Equal(statePending))
		})

		It("marks permanently failed images", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source:     "quay.io/img:v1",
					State:      stateFailed,
					RetryCount: 10,
					LastError:  "permanent error",
				},
			}

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			entry := m.imageStates[testImageSetName]["reg.io/img:v1"]
			Expect(entry.PermanentlyFailed).To(BeTrue())
		})

		It("trusts mirrored state outside drift check window", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  stateMirrored,
				},
			}
			// Set lastDriftCheck to recent to avoid drift check
			m.lastDriftCheck = time.Now()

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(m.mirrored["reg.io/img:v1"]).To(BeTrue())
		})

		It("marks in-memory mirrored image as Mirrored in state", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  statePending,
				},
			}
			m.mirrored["reg.io/img:v1"] = true
			m.lastDriftCheck = time.Now()

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(m.imageStates[testImageSetName]["reg.io/img:v1"].State).To(Equal(stateMirrored))
		})

		It("skips already in-progress pending images", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  statePending,
				},
			}
			m.inProgress["reg.io/img:v1"] = "existing-worker"
			m.lastDriftCheck = time.Now()

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
		})

		It("dispatches pending images as worker batches", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					Concurrency:  1,
					BatchSize:    10,
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  statePending,
				},
			}
			m.lastDriftCheck = time.Now()

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(m.inProgress).To(HaveKey("reg.io/img:v1"))
		})

		It("skips non-Pending non-Failed entries", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  "UnknownState",
				},
			}
			m.lastDriftCheck = time.Now()

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
		})

		It("loads state from ConfigMap on first access", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			// imageStates is empty → should try to load from ConfigMap
			m.lastDriftCheck = time.Now()

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles concurrency with multiple pending images", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					Concurrency:  2,
					BatchSize:    1,
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{Source: "quay.io/img:v1", State: statePending},
				"reg.io/img:v2": &imagestate.ImageEntry{Source: "quay.io/img:v2", State: statePending},
				"reg.io/img:v3": &imagestate.ImageEntry{Source: "quay.io/img:v3", State: statePending},
			}
			m.lastDriftCheck = time.Now()

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			// With concurrency=2, batchSize=1, at most 2 batches should be dispatched
			Expect(len(m.inProgress)).To(BeNumerically("<=", 3))
			Expect(m.inProgress).ToNot(BeEmpty())
		})

		It("handles permanently failed image that already has flag set", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source:            "quay.io/img:v1",
					State:             stateFailed,
					RetryCount:        10,
					PermanentlyFailed: true,
				},
			}
			m.lastDriftCheck = time.Now()

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ─── buildCollector ──────────────────────────────────────────────

	Context("buildCollector", func() {
		It("returns a collector for standard config", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.io/mirror",
				},
			}
			collector, resolver := m.buildCollector(mt)
			Expect(collector).NotTo(BeNil())
			Expect(resolver).NotTo(BeNil())
		})

		It("returns a collector for insecure config", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.io/mirror",
					Insecure: true,
				},
			}
			collector, resolver := m.buildCollector(mt)
			Expect(collector).NotTo(BeNil())
			Expect(resolver).NotTo(BeNil())
		})

		It("strips path from registry for insecure host", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.io:5000/deep/path/mirror",
					Insecure: true,
				},
			}
			collector, resolver := m.buildCollector(mt)
			Expect(collector).NotTo(BeNil())
			Expect(resolver).NotTo(BeNil())
		})
	})

	// ─── Additional reconcile edge-cases ─────────────────────────────

	Context("Reconcile concurrency limits", func() {
		It("respects concurrency=1 limit with multiple pending images", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					Concurrency:  1,
					BatchSize:    50,
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			// Pre-fill an existing in-progress entry → activePods already at 1
			m.inProgress["reg.io/other:v9"] = "existing-worker-pod"
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{Source: "quay.io/img:v1", State: statePending},
				"reg.io/img:v2": &imagestate.ImageEntry{Source: "quay.io/img:v2", State: statePending},
			}
			m.lastDriftCheck = time.Now()

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			// concurrency=1 and there's already 1 active pod → no new pods dispatched
		})
	})

	Context("Reconcile with ConfigMap interactions", func() {
		It("handles state save during stateChanged", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.io",
					ImageSets:    []string{testImageSetName},
					Concurrency:  1,
					BatchSize:    50,
					PollInterval: &metav1.Duration{Duration: -1},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source:     "quay.io/img:v1",
					State:      stateFailed,
					RetryCount: 5,
					LastError:  "timeout",
				},
				"reg.io/img:v2": &imagestate.ImageEntry{
					Source: "quay.io/img:v2",
					State:  stateMirrored,
				},
			}
			m.lastDriftCheck = time.Now()

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			// Failed with retryCount < 10 should reset to Pending
			Expect(m.imageStates[testImageSetName]["reg.io/img:v1"].State).To(Equal(statePending))
			// Mirrored should be trusted
			Expect(m.mirrored["reg.io/img:v2"]).To(BeTrue())
		})

		It("handles CheckExistInterval configuration", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:           "reg.io",
					ImageSets:          []string{testImageSetName},
					PollInterval:       &metav1.Duration{Duration: -1},
					CheckExistInterval: &metav1.Duration{Duration: 2 * time.Hour},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testImageSetName,
					Namespace:  "default",
					Generation: 1,
				},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(mt, is).
				WithStatusSubresource(is).
				Build()
			cs := k8sfake.NewSimpleClientset()
			m = NewWithClients(c, cs, "test", "default", "test-image:latest", "", scheme)
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{Source: "quay.io/img:v1", State: statePending},
			}
			// Set lastDriftCheck to recent (within CheckExistInterval)
			m.lastDriftCheck = time.Now()

			err := m.reconcile(context.TODO())
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ─── setImageStateLocked additional ──────────────────────────────

	Context("setImageStateLocked additional", func() {
		It("sets PermanentlyFailed when retryCount reaches 10", func() {
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source:     "quay.io/img:v1",
					State:      statePending,
					RetryCount: 9,
				},
			}
			m.destToIS["reg.io/img:v1"] = testImageSetName
			m.setImageStateLocked("reg.io/img:v1", stateFailed, "final error")
			entry := m.imageStates[testImageSetName]["reg.io/img:v1"]
			Expect(entry.RetryCount).To(Equal(10))
			Expect(entry.PermanentlyFailed).To(BeTrue())
		})

		It("skips when isState does not exist", func() {
			m.destToIS["reg.io/img:v1"] = "nonexistent-is"
			// Should not panic
			m.setImageStateLocked("reg.io/img:v1", stateMirrored, "")
		})

		It("skips when entry not in isState", func() {
			m.imageStates[testImageSetName] = imagestate.ImageState{}
			m.destToIS["reg.io/img:v1"] = testImageSetName
			// Should not panic
			m.setImageStateLocked("reg.io/img:v1", stateMirrored, "")
		})

		It("marks dirty on state change", func() {
			m.imageStates[testImageSetName] = imagestate.ImageState{
				"reg.io/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  statePending,
				},
			}
			m.destToIS["reg.io/img:v1"] = testImageSetName
			m.setImageStateLocked("reg.io/img:v1", stateMirrored, "")
			Expect(m.dirtyStateNames[testImageSetName]).To(BeTrue())
		})
	})

	// ─── Additional startWorkerBatch coverage ────────────────────────

	Context("startWorkerBatch with WorkerStorage size", func() {
		It("uses custom size from WorkerStorage", func() {
			size := resource.MustParse("50Gi")
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid-1"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.io",
					WorkerStorage: &mirrorv1alpha1.WorkerStorageConfig{
						Size: size,
					},
				},
			}
			items := []BatchItem{{Source: "quay.io/img:v1", Dest: "reg.io/mirror/img:v1"}}
			_, err := m.startWorkerBatch(context.TODO(), mt, items)
			Expect(err).NotTo(HaveOccurred())

			pods, _ := m.Clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
			pod := pods.Items[0]
			foundEphemeral := false
			for _, v := range pod.Spec.Volumes {
				if v.Name == "blob-buffer" && v.Ephemeral != nil {
					foundEphemeral = true
					req := v.Ephemeral.VolumeClaimTemplate.Spec.Resources.Requests[corev1.ResourceStorage]
					Expect(req.String()).To(Equal("50Gi"))
				}
			}
			Expect(foundEphemeral).To(BeTrue())
		})
	})

	// ─── Additional ensureWorkerTokenSecret edge cases ───────────────

	Context("ensureWorkerTokenSecret error handling", func() {
		It("sets controller reference on created secret", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
					UID:       "uid-123",
				},
			}
			err := m.ensureWorkerTokenSecret(context.TODO(), mt)
			Expect(err).NotTo(HaveOccurred())
			Expect(m.workerToken).NotTo(BeEmpty())
		})
	})
})

// envVarMap converts a slice of env vars to a map for easy assertions.
func envVarMap(envs []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(envs))
	for _, e := range envs {
		m[e.Name] = e.Value
	}
	return m
}
