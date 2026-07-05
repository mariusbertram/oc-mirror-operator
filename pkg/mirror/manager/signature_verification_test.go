package manager

import (
	"context"
	"strings"
	"testing"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestVerifyOperatorCatalogSignature_SecretNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	m := &MirrorManager{
		Client:     fake.NewClientBuilder().WithScheme(scheme).Build(),
		Namespace:  "test-ns",
		TargetName: "test-target",
	}
	mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "registry.example.com/mirror"}}
	op := mirrorv1alpha1.Operator{
		Catalog: "registry.example.com/catalog:v1",
		SignatureVerification: &mirrorv1alpha1.CosignVerification{
			PublicKeySecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "missing-secret"},
				Key:                  "cosign.pub",
			},
		},
	}

	err := m.verifyOperatorCatalogSignature(context.Background(), mt, op, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
	if err == nil {
		t.Fatal("expected error when the public key Secret does not exist")
	}
	if !strings.Contains(err.Error(), "get public key secret") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestVerifyOperatorCatalogSignature_SecretMissingKey(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cosign-key", Namespace: "test-ns"},
		Data:       map[string][]byte{"other-key": []byte("irrelevant")},
	}
	m := &MirrorManager{
		Client:     fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		Namespace:  "test-ns",
		TargetName: "test-target",
	}
	mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "registry.example.com/mirror"}}
	op := mirrorv1alpha1.Operator{
		Catalog: "registry.example.com/catalog:v1",
		SignatureVerification: &mirrorv1alpha1.CosignVerification{
			PublicKeySecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cosign-key"},
				Key:                  "cosign.pub",
			},
		},
	}

	err := m.verifyOperatorCatalogSignature(context.Background(), mt, op, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
	if err == nil {
		t.Fatal("expected error when the Secret has no data for the referenced key")
	}
	if !strings.Contains(err.Error(), `no key "cosign.pub"`) {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestVerifyOperatorCatalogSignature_InvalidPublicKey(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cosign-key", Namespace: "test-ns"},
		Data:       map[string][]byte{"cosign.pub": []byte("not a pem key")},
	}
	m := &MirrorManager{
		Client:     fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		Namespace:  "test-ns",
		TargetName: "test-target",
	}
	mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "registry.example.com/mirror"}}
	op := mirrorv1alpha1.Operator{
		Catalog: "registry.example.com/catalog:v1",
		SignatureVerification: &mirrorv1alpha1.CosignVerification{
			PublicKeySecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cosign-key"},
				Key:                  "cosign.pub",
			},
		},
	}

	err := m.verifyOperatorCatalogSignature(context.Background(), mt, op, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
	if err == nil {
		t.Fatal("expected error for an invalid PEM public key")
	}
}
