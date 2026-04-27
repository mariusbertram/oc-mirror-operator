/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package manager

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

var _ = Describe("MirrorManager logic", func() {
	var (
		m      *MirrorManager
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		_ = mirrorv1alpha1.AddToScheme(scheme)
		_ = corev1.AddToScheme(scheme)

		m = &MirrorManager{
			imageState: make(imagestate.ImageState),
		}
	})

	Context("operatorCacheHit", func() {
		It("should match when version and digest are identical", func() {
			cached := operatorCacheValue("sha256:abc123")
			Expect(operatorCacheHit(cached, "sha256:abc123")).To(BeTrue())
		})

		It("should NOT match when digest differs", func() {
			cached := operatorCacheValue("sha256:abc123")
			Expect(operatorCacheHit(cached, "sha256:different")).To(BeFalse())
		})

		It("should NOT match when version prefix is missing", func() {
			Expect(operatorCacheHit("sha256:abc123", "sha256:abc123")).To(BeFalse())
		})

		It("should NOT match empty cached value", func() {
			Expect(operatorCacheHit("", "sha256:abc123")).To(BeFalse())
		})
	})

	Context("setImageStateLocked", func() {
		It("should update entry state directly", func() {
			m.imageState = imagestate.ImageState{
				"reg.io/mirror/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  statePending,
				},
			}
			m.setImageStateLocked("reg.io/mirror/img:v1", stateMirrored, "")

			entry := m.imageState["reg.io/mirror/img:v1"]
			Expect(entry.State).To(Equal(stateMirrored))
			Expect(m.stateDirty).To(BeTrue())
		})

		It("should be idempotent for duplicate callbacks", func() {
			m.imageState = imagestate.ImageState{
				"reg.io/mirror/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  stateMirrored,
				},
			}
			m.stateDirty = false
			m.setImageStateLocked("reg.io/mirror/img:v1", stateMirrored, "")
			Expect(m.stateDirty).To(BeFalse())
		})

		It("should increment retry count on failure", func() {
			m.imageState = imagestate.ImageState{
				"reg.io/mirror/img:v1": &imagestate.ImageEntry{
					Source: "quay.io/img:v1",
					State:  statePending,
				},
			}
			m.setImageStateLocked("reg.io/mirror/img:v1", stateFailed, "timeout")

			entry := m.imageState["reg.io/mirror/img:v1"]
			Expect(entry.RetryCount).To(Equal(1))
			Expect(entry.State).To(Equal(stateFailed))

			// New failure with different error
			m.setImageStateLocked("reg.io/mirror/img:v1", stateFailed, "connection refused")
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

		It("should return true when any catalog annotation is missing the version prefix", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						mirrorv1alpha1.CatalogDigestAnnotationPrefix + "abc": "sha256:xyz",
					},
				},
			}
			Expect(hasStaleCacheAnnotations(is)).To(BeTrue())
		})

		It("should return true when any catalog annotation has old version prefix", func() {
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						mirrorv1alpha1.CatalogDigestAnnotationPrefix + "abc": "v1:sha256:xyz",
					},
				},
			}
			Expect(hasStaleCacheAnnotations(is)).To(BeTrue())
		})
	})

	Context("copyMap", func() {
		It("should return a deep copy", func() {
			in := map[string]string{"k1": "v1"}
			out := copyMap(in)
			Expect(out).To(Equal(in))
			out["k1"] = "changed"
			Expect(in["k1"]).To(Equal("v1"))
		})
	})
})
