package mirror

import (
	"strings"
	"testing"
)

func TestGenerateIDMS(t *testing.T) {
	images := []TargetImage{
		{
			Source:      "quay.io/ocp-mirror/test@sha256:45b41094054a1656837966006c9a3099991234567890abcdef1234567890abcd",
			Destination: "internal.registry.com/mirror/test:sha256-45b41094054a1656837966006c9a3099991234567890abcdef1234567890abcd",
		},
	}

	data, err := GenerateIDMS("test-idms", images)
	if err != nil {
		t.Fatalf("Failed to generate IDMS: %v", err)
	}

	yamlStr := string(data)
	if !strings.Contains(yamlStr, "ImageDigestMirrorSet") {
		t.Errorf("Expected ImageDigestMirrorSet, got %s", yamlStr)
	}
	if !strings.Contains(yamlStr, "quay.io/ocp-mirror/test") {
		t.Errorf("Expected source quay.io/ocp-mirror/test, got %s", yamlStr)
	}
}

func TestGenerateITMS(t *testing.T) {
	images := []TargetImage{
		{
			Source:      "quay.io/ocp-mirror/test:v1",
			Destination: "internal.registry.com/mirror/test:v1",
		},
	}

	data, err := GenerateITMS("test-itms", images)
	if err != nil {
		t.Fatalf("Failed to generate ITMS: %v", err)
	}

	yamlStr := string(data)
	if !strings.Contains(yamlStr, "ImageTagMirrorSet") {
		t.Errorf("Expected ImageTagMirrorSet, got %s", yamlStr)
	}
}
