package resources

import (
	"encoding/base64"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// yamlMap is a convenience type for unmarshalling YAML into a generic map.
type yamlMap = map[string]interface{}

var _ = Describe("splitImageRef", func() {
	DescribeTable("correctly splits image references",
		func(ref, expectedRepo, expectedTagOrDigest string) {
			repo, tagOrDigest := splitImageRef(ref)
			Expect(repo).To(Equal(expectedRepo))
			Expect(tagOrDigest).To(Equal(expectedTagOrDigest))
		},
		Entry("digest ref",
			"registry.example.com/repo@sha256:abc123",
			"registry.example.com/repo", "@sha256:abc123"),
		Entry("tag ref",
			"registry.example.com/repo:v1.0",
			"registry.example.com/repo", ":v1.0"),
		Entry("no tag or digest",
			"registry.example.com/repo",
			"registry.example.com/repo", ""),
		Entry("registry with port and tag",
			"registry.example.com:5000/repo:v1.0",
			"registry.example.com:5000/repo", ":v1.0"),
		Entry("registry with port, no tag",
			"registry.example.com:5000/repo",
			"registry.example.com:5000/repo", ""),
		Entry("registry with port and digest",
			"registry.example.com:5000/repo@sha256:deadbeef",
			"registry.example.com:5000/repo", "@sha256:deadbeef"),
		Entry("deeply nested repo with tag",
			"registry.io/org/sub/image:latest",
			"registry.io/org/sub/image", ":latest"),
		Entry("deeply nested repo with port and tag",
			"registry.io:8443/org/sub/image:latest",
			"registry.io:8443/org/sub/image", ":latest"),
	)
})

var _ = Describe("repoOnly", func() {
	DescribeTable("strips tag or digest",
		func(ref, expected string) {
			Expect(repoOnly(ref)).To(Equal(expected))
		},
		Entry("digest ref", "registry.example.com/repo@sha256:abc123", "registry.example.com/repo"),
		Entry("tag ref", "registry.example.com/repo:v1.0", "registry.example.com/repo"),
		Entry("no tag or digest", "registry.example.com/repo", "registry.example.com/repo"),
		Entry("port and tag", "registry.example.com:5000/repo:v1.0", "registry.example.com:5000/repo"),
		Entry("port, no tag", "registry.example.com:5000/repo", "registry.example.com:5000/repo"),
	)
})

var _ = Describe("isDigestRef", func() {
	DescribeTable("detects digest references",
		func(ref string, expected bool) {
			Expect(isDigestRef(ref)).To(Equal(expected))
		},
		Entry("digest ref", "registry/repo@sha256:abc", true),
		Entry("tag ref", "registry/repo:v1.0", false),
		Entry("bare ref", "registry/repo", false),
	)
})

var _ = Describe("isTagRef", func() {
	DescribeTable("detects tag references",
		func(ref string, expected bool) {
			Expect(isTagRef(ref)).To(Equal(expected))
		},
		Entry("tag ref", "registry/repo:v1.0", true),
		Entry("digest ref", "registry/repo@sha256:abc", false),
		Entry("bare ref", "registry/repo", false),
		// Note: isTagRef uses simple string heuristic — "registry:5000/repo"
		// contains ":" and no "@", so it returns true. This is a known
		// limitation; real callers only pass fully qualified refs.
		Entry("port-only (has colon)", "registry:5000/repo", true),
	)
})

var _ = Describe("GenerateIDMS", func() {
	It("produces an empty IDMS for empty state", func() {
		out, err := GenerateIDMS("test-idms", imagestate.ImageState{})
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		Expect(m["apiVersion"]).To(Equal("config.openshift.io/v1"))
		Expect(m["kind"]).To(Equal("ImageDigestMirrorSet"))

		spec := m["spec"].(yamlMap)
		Expect(spec["imageDigestMirrors"]).To(BeEmpty())
	})

	It("generates correct mirror entries for Mirrored digest images", func() {
		state := imagestate.ImageState{
			"mirror.example.com/repo@sha256:abc123": &imagestate.ImageEntry{
				Source: "source.example.com/repo@sha256:abc123",
				State:  "Mirrored",
			},
		}
		out, err := GenerateIDMS("test-idms", state)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())

		meta := m["metadata"].(yamlMap)
		Expect(meta["name"]).To(Equal("test-idms"))

		spec := m["spec"].(yamlMap)
		mirrors := spec["imageDigestMirrors"].([]interface{})
		Expect(mirrors).To(HaveLen(1))

		entry := mirrors[0].(yamlMap)
		Expect(entry["source"]).To(Equal("source.example.com/repo"))
		entryMirrors := entry["mirrors"].([]interface{})
		Expect(entryMirrors).To(ContainElement("mirror.example.com/repo"))
	})

	It("skips non-Mirrored entries", func() {
		state := imagestate.ImageState{
			"mirror.example.com/repo@sha256:abc123": &imagestate.ImageEntry{
				Source: "source.example.com/repo@sha256:abc123",
				State:  "Pending",
			},
			"mirror.example.com/repo2@sha256:def456": &imagestate.ImageEntry{
				Source: "source.example.com/repo2@sha256:def456",
				State:  "Failed",
			},
		}
		out, err := GenerateIDMS("test-idms", state)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		spec := m["spec"].(yamlMap)
		Expect(spec["imageDigestMirrors"]).To(BeEmpty())
	})

	It("skips tag-based refs (only digests go to IDMS)", func() {
		state := imagestate.ImageState{
			"mirror.example.com/repo:v1.0": &imagestate.ImageEntry{
				Source: "source.example.com/repo:v1.0",
				State:  "Mirrored",
			},
		}
		out, err := GenerateIDMS("test-idms", state)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		spec := m["spec"].(yamlMap)
		Expect(spec["imageDigestMirrors"]).To(BeEmpty())
	})

	It("skips entries where source repo equals destination repo", func() {
		state := imagestate.ImageState{
			"registry.example.com/repo@sha256:abc123": &imagestate.ImageEntry{
				Source: "registry.example.com/repo@sha256:abc123",
				State:  "Mirrored",
			},
		}
		out, err := GenerateIDMS("test-idms", state)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		spec := m["spec"].(yamlMap)
		Expect(spec["imageDigestMirrors"]).To(BeEmpty())
	})

	It("produces deterministically sorted output for multiple sources", func() {
		state := imagestate.ImageState{
			"mirror.example.com/z-repo@sha256:111": &imagestate.ImageEntry{
				Source: "upstream.io/z-repo@sha256:111",
				State:  "Mirrored",
			},
			"mirror.example.com/a-repo@sha256:222": &imagestate.ImageEntry{
				Source: "upstream.io/a-repo@sha256:222",
				State:  "Mirrored",
			},
		}
		out, err := GenerateIDMS("sorted-idms", state)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		spec := m["spec"].(yamlMap)
		mirrors := spec["imageDigestMirrors"].([]interface{})
		Expect(mirrors).To(HaveLen(2))
		// a-repo should come first
		Expect(mirrors[0].(yamlMap)["source"]).To(Equal("upstream.io/a-repo"))
		Expect(mirrors[1].(yamlMap)["source"]).To(Equal("upstream.io/z-repo"))
	})

	It("merges multiple destinations into a single source entry", func() {
		state := imagestate.ImageState{
			"mirror1.example.com/repo@sha256:abc": &imagestate.ImageEntry{
				Source: "upstream.io/repo@sha256:abc",
				State:  "Mirrored",
			},
			"mirror2.example.com/repo@sha256:abc": &imagestate.ImageEntry{
				Source: "upstream.io/repo@sha256:abc",
				State:  "Mirrored",
			},
		}
		out, err := GenerateIDMS("merge-idms", state)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		spec := m["spec"].(yamlMap)
		mirrors := spec["imageDigestMirrors"].([]interface{})
		Expect(mirrors).To(HaveLen(1))
		entryMirrors := mirrors[0].(yamlMap)["mirrors"].([]interface{})
		Expect(entryMirrors).To(HaveLen(2))
	})
})

var _ = Describe("GenerateITMS", func() {
	It("produces an empty ITMS for empty state", func() {
		out, err := GenerateITMS("test-itms", imagestate.ImageState{})
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		Expect(m["apiVersion"]).To(Equal("config.openshift.io/v1"))
		Expect(m["kind"]).To(Equal("ImageTagMirrorSet"))
		spec := m["spec"].(yamlMap)
		Expect(spec["imageTagMirrors"]).To(BeEmpty())
	})

	It("generates correct mirror entries for Mirrored tag images", func() {
		state := imagestate.ImageState{
			"mirror.example.com/repo:v1.0": &imagestate.ImageEntry{
				Source: "source.example.com/repo:v1.0",
				State:  "Mirrored",
			},
		}
		out, err := GenerateITMS("test-itms", state)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())

		meta := m["metadata"].(yamlMap)
		Expect(meta["name"]).To(Equal("test-itms"))

		spec := m["spec"].(yamlMap)
		mirrors := spec["imageTagMirrors"].([]interface{})
		Expect(mirrors).To(HaveLen(1))

		entry := mirrors[0].(yamlMap)
		Expect(entry["source"]).To(Equal("source.example.com/repo"))
		entryMirrors := entry["mirrors"].([]interface{})
		Expect(entryMirrors).To(ContainElement("mirror.example.com/repo"))
	})

	It("skips digest refs (only tags go to ITMS)", func() {
		state := imagestate.ImageState{
			"mirror.example.com/repo@sha256:abc": &imagestate.ImageEntry{
				Source: "source.example.com/repo@sha256:abc",
				State:  "Mirrored",
			},
		}
		out, err := GenerateITMS("test-itms", state)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		spec := m["spec"].(yamlMap)
		Expect(spec["imageTagMirrors"]).To(BeEmpty())
	})

	It("skips non-Mirrored entries", func() {
		state := imagestate.ImageState{
			"mirror.example.com/repo:v1.0": &imagestate.ImageEntry{
				Source: "source.example.com/repo:v1.0",
				State:  "Pending",
			},
		}
		out, err := GenerateITMS("test-itms", state)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		spec := m["spec"].(yamlMap)
		Expect(spec["imageTagMirrors"]).To(BeEmpty())
	})

	It("skips self-mirror entries", func() {
		state := imagestate.ImageState{
			"registry.example.com/repo:v1.0": &imagestate.ImageEntry{
				Source: "registry.example.com/repo:v1.0",
				State:  "Mirrored",
			},
		}
		out, err := GenerateITMS("test-itms", state)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		spec := m["spec"].(yamlMap)
		Expect(spec["imageTagMirrors"]).To(BeEmpty())
	})

	It("produces sorted output for multiple sources", func() {
		state := imagestate.ImageState{
			"mirror.example.com/z-repo:v1": &imagestate.ImageEntry{
				Source: "upstream.io/z-repo:v1",
				State:  "Mirrored",
			},
			"mirror.example.com/a-repo:v2": &imagestate.ImageEntry{
				Source: "upstream.io/a-repo:v2",
				State:  "Mirrored",
			},
		}
		out, err := GenerateITMS("sorted-itms", state)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		spec := m["spec"].(yamlMap)
		mirrors := spec["imageTagMirrors"].([]interface{})
		Expect(mirrors).To(HaveLen(2))
		Expect(mirrors[0].(yamlMap)["source"]).To(Equal("upstream.io/a-repo"))
		Expect(mirrors[1].(yamlMap)["source"]).To(Equal("upstream.io/z-repo"))
	})
})

var _ = Describe("GenerateCatalogSource", func() {
	catalog := CatalogInfo{
		SourceCatalog: "registry.redhat.io/redhat/redhat-operator-index:v4.21",
		TargetImage:   "mirror.example.com/redhat-operator-index:v4.21",
		DisplayName:   "Red Hat Operators",
	}

	It("generates valid CatalogSource YAML with all fields", func() {
		out, err := GenerateCatalogSource("my-catalog", "openshift-marketplace", catalog, "my-pull-secret")
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())

		Expect(m["apiVersion"]).To(Equal("operators.coreos.com/v1alpha1"))
		Expect(m["kind"]).To(Equal("CatalogSource"))

		meta := m["metadata"].(yamlMap)
		Expect(meta["name"]).To(Equal("my-catalog"))
		Expect(meta["namespace"]).To(Equal("openshift-marketplace"))

		spec := m["spec"].(yamlMap)
		Expect(spec["sourceType"]).To(Equal("grpc"))
		Expect(spec["image"]).To(Equal("mirror.example.com/redhat-operator-index:v4.21"))
		Expect(spec["displayName"]).To(Equal("Red Hat Operators"))
		Expect(spec["publisher"]).To(Equal("oc-mirror-operator"))

		// Verify secrets field is present
		secrets := spec["secrets"].([]interface{})
		Expect(secrets).To(ContainElement("my-pull-secret"))

		// Verify updateStrategy
		updateStrategy := spec["updateStrategy"].(yamlMap)
		registryPoll := updateStrategy["registryPoll"].(yamlMap)
		Expect(registryPoll["interval"]).To(Equal("10m"))
	})

	It("omits secrets field when pullSecretName is empty", func() {
		out, err := GenerateCatalogSource("my-catalog", "openshift-marketplace", catalog, "")
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		spec := m["spec"].(yamlMap)
		Expect(spec).NotTo(HaveKey("secrets"))
	})
})

var _ = Describe("GenerateClusterCatalog", func() {
	It("generates valid ClusterCatalog YAML", func() {
		catalog := CatalogInfo{
			SourceCatalog: "registry.redhat.io/redhat/redhat-operator-index:v4.21",
			TargetImage:   "mirror.example.com/redhat-operator-index:v4.21",
			DisplayName:   "Red Hat Operators",
		}

		out, err := GenerateClusterCatalog("my-cluster-catalog", catalog)
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())

		Expect(m["apiVersion"]).To(Equal("olm.operatorframework.io/v1"))
		Expect(m["kind"]).To(Equal("ClusterCatalog"))

		meta := m["metadata"].(yamlMap)
		Expect(meta["name"]).To(Equal("my-cluster-catalog"))

		spec := m["spec"].(yamlMap)
		source := spec["source"].(yamlMap)
		Expect(source["type"]).To(Equal("Image"))
		image := source["image"].(yamlMap)
		Expect(image["ref"]).To(Equal("mirror.example.com/redhat-operator-index:v4.21"))
	})
})

var _ = Describe("GenerateSignatureConfigMaps", func() {
	It("returns a comment for empty signatures", func() {
		out, err := GenerateSignatureConfigMaps(SignatureData{})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal("# No release signatures available\n"))
	})

	It("returns a comment for nil signatures", func() {
		out, err := GenerateSignatureConfigMaps(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal("# No release signatures available\n"))
	})

	It("generates a valid ConfigMap for a single signature", func() {
		digest := "sha256:aabbccddee112233445566778899aabbccddee112233445566778899aabbccdd"
		sigData := []byte("fake-signature-data")

		out, err := GenerateSignatureConfigMaps(SignatureData{digest: sigData})
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())

		Expect(m["apiVersion"]).To(Equal("v1"))
		Expect(m["kind"]).To(Equal("ConfigMap"))

		meta := m["metadata"].(yamlMap)
		// Name: sha256-<first 12 hex chars>-1
		Expect(meta["name"]).To(Equal("sha256-aabbccddee11-1"))
		Expect(meta["namespace"]).To(Equal("openshift-config-managed"))

		labels := meta["labels"].(yamlMap)
		Expect(labels).To(HaveKey("release.openshift.io/verification-signatures"))

		// binaryData key: sha256-<full hash>-1
		hashPart := "aabbccddee112233445566778899aabbccddee112233445566778899aabbccdd"
		expectedKey := "sha256-" + hashPart + "-1"
		binaryData := m["binaryData"].(yamlMap)
		Expect(binaryData).To(HaveKey(expectedKey))

		// The value is base64-encoded by YAML marshalling of []byte
		b64Value := binaryData[expectedKey].(string)
		decoded, err := base64.StdEncoding.DecodeString(b64Value)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(Equal(sigData))
	})

	It("generates multi-document YAML for multiple signatures, sorted by digest", func() {
		sigs := SignatureData{
			"sha256:zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz1": []byte("sig-z"),
			"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": []byte("sig-a"),
		}
		out, err := GenerateSignatureConfigMaps(sigs)
		Expect(err).NotTo(HaveOccurred())

		// Split on the multi-document separator
		raw := string(out)
		Expect(raw).To(ContainSubstring("---\n"))

		// Parse each document — split on "---\n"
		docs := splitYAMLDocs(raw)
		Expect(docs).To(HaveLen(2))

		var first, second yamlMap
		Expect(yaml.Unmarshal([]byte(docs[0]), &first)).To(Succeed())
		Expect(yaml.Unmarshal([]byte(docs[1]), &second)).To(Succeed())

		// "aaaa..." sorts before "zzzz..."
		Expect(first["metadata"].(yamlMap)["name"]).To(Equal("sha256-aaaaaaaaaaaa-1"))
		Expect(second["metadata"].(yamlMap)["name"]).To(HavePrefix("sha256-zzzzzzzzzzzz"))
	})
})

var _ = Describe("GenerateSignatureConfigMapsBase64", func() {
	It("decodes base64 and delegates to GenerateSignatureConfigMaps", func() {
		sigBytes := []byte("test-signature")
		b64 := base64.StdEncoding.EncodeToString(sigBytes)
		digest := "sha256:aabbccddee112233445566778899aabbccddee112233445566778899aabbccdd"

		out, err := GenerateSignatureConfigMapsBase64(map[string]string{digest: b64})
		Expect(err).NotTo(HaveOccurred())

		var m yamlMap
		Expect(yaml.Unmarshal(out, &m)).To(Succeed())
		Expect(m["kind"]).To(Equal("ConfigMap"))

		hashPart := "aabbccddee112233445566778899aabbccddee112233445566778899aabbccdd"
		expectedKey := "sha256-" + hashPart + "-1"
		binaryData := m["binaryData"].(yamlMap)
		Expect(binaryData).To(HaveKey(expectedKey))

		// Verify the actual signature bytes
		b64Value := binaryData[expectedKey].(string)
		decoded, err := base64.StdEncoding.DecodeString(b64Value)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(Equal(sigBytes))
	})

	It("returns an error for invalid base64", func() {
		_, err := GenerateSignatureConfigMapsBase64(map[string]string{
			"sha256:abc123": "!!!not-valid-base64!!!",
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to decode signature"))
	})

	It("returns empty comment for empty map", func() {
		out, err := GenerateSignatureConfigMapsBase64(map[string]string{})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal("# No release signatures available\n"))
	})
})

// splitYAMLDocs splits a multi-document YAML string by "---\n".
// It handles the fact that the join uses "---\n" between docs.
func splitYAMLDocs(raw string) []string {
	parts := make([]string, 0)
	// The generator joins with "---\n", so split on that
	for _, chunk := range splitOn(raw, "---\n") {
		trimmed := trimSpaceAndNewlines(chunk)
		if trimmed != "" {
			parts = append(parts, chunk)
		}
	}
	return parts
}

func splitOn(s, sep string) []string {
	var result []string
	for {
		idx := indexOf(s, sep)
		if idx == -1 {
			result = append(result, s)
			break
		}
		result = append(result, s[:idx])
		s = s[idx+len(sep):]
	}
	return result
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func trimSpaceAndNewlines(s string) string {
	result := s
	for len(result) > 0 && (result[0] == ' ' || result[0] == '\n' || result[0] == '\r' || result[0] == '\t') {
		result = result[1:]
	}
	for len(result) > 0 && (result[len(result)-1] == ' ' || result[len(result)-1] == '\n' || result[len(result)-1] == '\r' || result[len(result)-1] == '\t') {
		result = result[:len(result)-1]
	}
	return result
}

// Ensure json import is used (for potential future assertions).
var _ = json.Unmarshal
