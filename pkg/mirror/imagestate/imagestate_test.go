package imagestate

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestDecode_CorruptGzipReturnsError(t *testing.T) {
	cm := &corev1.ConfigMap{
		BinaryData: map[string][]byte{
			"images.json.gz": []byte("not-actually-gzip-data"),
		},
	}
	state, err := decode(cm)
	if err == nil {
		t.Fatalf("expected error decoding corrupt gzip, got state=%v", state)
	}
	if state != nil {
		t.Fatalf("expected nil state on error, got %v", state)
	}
}

func TestDecode_CorruptJSONReturnsError(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"images.json": "{not-valid-json",
		},
	}
	state, err := decode(cm)
	if err == nil {
		t.Fatalf("expected error decoding corrupt json, got state=%v", state)
	}
	if state != nil {
		t.Fatalf("expected nil state on error, got %v", state)
	}
}

func TestDecode_EmptyConfigMapReturnsEmptyState(t *testing.T) {
	cm := &corev1.ConfigMap{}
	state, err := decode(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(state) != 0 {
		t.Fatalf("expected empty state, got %v", state)
	}
}
