package builder

import (
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = batchv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = mirrorv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func newManager(image string) *CatalogBuildManager {
	return &CatalogBuildManager{operatorImage: image}
}

func defaultImageSet() *mirrorv1alpha1.ImageSet {
	return &mirrorv1alpha1.ImageSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: mirrorv1alpha1.GroupVersion.String(),
			Kind:       "ImageSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-is",
			Namespace: "test-ns",
			UID:       "abc-123",
		},
	}
}

func defaultMirrorTarget() *mirrorv1alpha1.MirrorTarget {
	return &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-mt",
			Namespace: "test-ns",
		},
		Spec: mirrorv1alpha1.MirrorTargetSpec{
			Registry: "registry.example.com/mirror",
		},
	}
}

func pkgs(names ...string) []mirrorv1alpha1.IncludePackage {
	out := make([]mirrorv1alpha1.IncludePackage, len(names))
	for i, n := range names {
		out[i] = mirrorv1alpha1.IncludePackage{Name: n}
	}
	return out
}

func op(catalog string, packages []mirrorv1alpha1.IncludePackage) mirrorv1alpha1.Operator {
	return mirrorv1alpha1.Operator{
		Catalog:       catalog,
		IncludeConfig: mirrorv1alpha1.IncludeConfig{Packages: packages},
	}
}

// ---------- resourcePtr ----------

var _ = Describe("resourcePtr", func() {
	It("returns a non-nil pointer to the parsed quantity", func() {
		q := resourcePtr("10Gi")
		Expect(q).NotTo(BeNil())
		Expect(q.String()).To(Equal("10Gi"))
	})

	It("correctly parses CPU quantities", func() {
		q := resourcePtr("500m")
		Expect(q).NotTo(BeNil())
		expected := resource.MustParse("500m")
		Expect(q.Cmp(expected)).To(Equal(0))
	})
})

// ---------- ptrBool ----------

var _ = Describe("ptrBool", func() {
	It("returns pointer to true", func() {
		p := ptrBool(true)
		Expect(p).NotTo(BeNil())
		Expect(*p).To(BeTrue())
	})

	It("returns pointer to false", func() {
		p := ptrBool(false)
		Expect(p).NotTo(BeNil())
		Expect(*p).To(BeFalse())
	})
})

// ---------- buildEffectiveNoProxy ----------

var _ = Describe("buildEffectiveNoProxy", func() {
	It("returns cluster defaults when user value is empty", func() {
		result := buildEffectiveNoProxy("")
		Expect(result).To(Equal("localhost,127.0.0.1,.svc,.svc.cluster.local"))
	})

	It("appends user value after cluster defaults", func() {
		result := buildEffectiveNoProxy("10.0.0.0/8,.corp.example.com")
		Expect(result).To(HavePrefix("localhost,127.0.0.1,.svc,.svc.cluster.local,"))
		Expect(result).To(HaveSuffix("10.0.0.0/8,.corp.example.com"))
	})
})

// ---------- catalogProxyEnvVars ----------

var _ = Describe("catalogProxyEnvVars", func() {
	It("returns nil when config is nil", func() {
		Expect(catalogProxyEnvVars(nil)).To(BeNil())
	})

	It("returns HTTP_PROXY vars when only HTTPProxy is set", func() {
		cfg := &mirrorv1alpha1.ProxyConfig{HTTPProxy: "http://proxy:3128"}
		env := catalogProxyEnvVars(cfg)
		envMap := envToMap(env)

		Expect(envMap).To(HaveKeyWithValue("HTTP_PROXY", "http://proxy:3128"))
		Expect(envMap).To(HaveKeyWithValue("http_proxy", "http://proxy:3128"))
		Expect(envMap).To(HaveKey("NO_PROXY"))
		Expect(envMap).To(HaveKey("no_proxy"))
		Expect(envMap).To(HaveKeyWithValue("KUBERNETES_SERVICE_HOST", "kubernetes.default.svc.cluster.local"))
		Expect(envMap).NotTo(HaveKey("HTTPS_PROXY"))
	})

	It("returns HTTPS_PROXY vars when only HTTPSProxy is set", func() {
		cfg := &mirrorv1alpha1.ProxyConfig{HTTPSProxy: "https://proxy:3129"}
		env := catalogProxyEnvVars(cfg)
		envMap := envToMap(env)

		Expect(envMap).To(HaveKeyWithValue("HTTPS_PROXY", "https://proxy:3129"))
		Expect(envMap).To(HaveKeyWithValue("https_proxy", "https://proxy:3129"))
		Expect(envMap).To(HaveKey("NO_PROXY"))
		Expect(envMap).NotTo(HaveKey("HTTP_PROXY"))
	})

	It("returns all proxy vars when both HTTP and HTTPS are set", func() {
		cfg := &mirrorv1alpha1.ProxyConfig{
			HTTPProxy:  "http://proxy:3128",
			HTTPSProxy: "https://proxy:3129",
			NoProxy:    "10.0.0.0/8",
		}
		env := catalogProxyEnvVars(cfg)
		envMap := envToMap(env)

		Expect(envMap).To(HaveKey("HTTP_PROXY"))
		Expect(envMap).To(HaveKey("HTTPS_PROXY"))
		Expect(envMap["NO_PROXY"]).To(ContainSubstring("localhost"))
		Expect(envMap["NO_PROXY"]).To(ContainSubstring("10.0.0.0/8"))
	})

	It("sets only NO_PROXY when only NoProxy is configured", func() {
		cfg := &mirrorv1alpha1.ProxyConfig{NoProxy: "internal.example.com"}
		env := catalogProxyEnvVars(cfg)
		envMap := envToMap(env)

		Expect(envMap).To(HaveKeyWithValue("NO_PROXY", "internal.example.com"))
		Expect(envMap).To(HaveKeyWithValue("no_proxy", "internal.example.com"))
		Expect(envMap).NotTo(HaveKey("HTTP_PROXY"))
		Expect(envMap).NotTo(HaveKey("HTTPS_PROXY"))
		Expect(envMap).NotTo(HaveKey("KUBERNETES_SERVICE_HOST"))
	})

	It("returns empty slice for empty ProxyConfig", func() {
		cfg := &mirrorv1alpha1.ProxyConfig{}
		env := catalogProxyEnvVars(cfg)
		Expect(env).To(BeEmpty())
	})
})

// ---------- JobName ----------

var _ = Describe("JobName", func() {
	It("produces a deterministic name", func() {
		a := JobName("my-is", "quay.io/catalog:v1")
		b := JobName("my-is", "quay.io/catalog:v1")
		Expect(a).To(Equal(b))
	})

	It("produces different names for different inputs", func() {
		a := JobName("is-a", "quay.io/catalog:v1")
		b := JobName("is-b", "quay.io/catalog:v1")
		Expect(a).NotTo(Equal(b))
	})
})

// ---------- BuildSignature ----------

var _ = Describe("BuildSignature", func() {
	var mgr *CatalogBuildManager

	BeforeEach(func() {
		mgr = newManager("registry.example.com/operator:v1")
	})

	It("returns a 16-char hex string", func() {
		sig := mgr.BuildSignature(nil)
		Expect(sig).To(HaveLen(16))
		Expect(sig).To(MatchRegexp("^[0-9a-f]{16}$"))
	})

	It("is deterministic for the same input", func() {
		ops := []mirrorv1alpha1.Operator{
			op("quay.io/catalog:v1", pkgs("pkg-a")),
		}
		Expect(mgr.BuildSignature(ops)).To(Equal(mgr.BuildSignature(ops)))
	})

	It("changes when operator image changes", func() {
		ops := []mirrorv1alpha1.Operator{{Catalog: "quay.io/catalog:v1"}}
		sig1 := mgr.BuildSignature(ops)
		mgr2 := newManager("registry.example.com/operator:v2")
		sig2 := mgr2.BuildSignature(ops)
		Expect(sig1).NotTo(Equal(sig2))
	})

	It("changes when packages change", func() {
		ops1 := []mirrorv1alpha1.Operator{op("cat:v1", pkgs("a"))}
		ops2 := []mirrorv1alpha1.Operator{op("cat:v1", pkgs("b"))}
		Expect(mgr.BuildSignature(ops1)).NotTo(Equal(mgr.BuildSignature(ops2)))
	})

	It("is order-independent for packages", func() {
		ops1 := []mirrorv1alpha1.Operator{op("cat:v1", pkgs("b", "a"))}
		ops2 := []mirrorv1alpha1.Operator{op("cat:v1", pkgs("a", "b"))}
		Expect(mgr.BuildSignature(ops1)).To(Equal(mgr.BuildSignature(ops2)))
	})

	It("skips operators with empty catalog", func() {
		ops1 := []mirrorv1alpha1.Operator{{Catalog: ""}}
		ops2 := []mirrorv1alpha1.Operator{}
		Expect(mgr.BuildSignature(ops1)).To(Equal(mgr.BuildSignature(ops2)))
	})

	It("includes Full flag in signature", func() {
		ops1 := []mirrorv1alpha1.Operator{{Catalog: "cat:v1", Full: false}}
		ops2 := []mirrorv1alpha1.Operator{{Catalog: "cat:v1", Full: true}}
		Expect(mgr.BuildSignature(ops1)).NotTo(Equal(mgr.BuildSignature(ops2)))
	})

	It("includes min version in signature", func() {
		ops1 := []mirrorv1alpha1.Operator{
			{Catalog: "cat:v1", IncludeConfig: mirrorv1alpha1.IncludeConfig{
				Packages: []mirrorv1alpha1.IncludePackage{
					{Name: "pkg", IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "1.0"}},
				},
			}},
		}
		ops2 := []mirrorv1alpha1.Operator{
			{Catalog: "cat:v1", IncludeConfig: mirrorv1alpha1.IncludeConfig{
				Packages: []mirrorv1alpha1.IncludePackage{
					{Name: "pkg", IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "2.0"}},
				},
			}},
		}
		Expect(mgr.BuildSignature(ops1)).NotTo(Equal(mgr.BuildSignature(ops2)))
	})

	It("includes max version in signature", func() {
		ops1 := []mirrorv1alpha1.Operator{
			{Catalog: "cat:v1", IncludeConfig: mirrorv1alpha1.IncludeConfig{
				Packages: []mirrorv1alpha1.IncludePackage{
					{Name: "pkg", IncludeBundle: mirrorv1alpha1.IncludeBundle{MaxVersion: "3.0"}},
				},
			}},
		}
		ops2 := []mirrorv1alpha1.Operator{
			{Catalog: "cat:v1", IncludeConfig: mirrorv1alpha1.IncludeConfig{
				Packages: []mirrorv1alpha1.IncludePackage{
					{Name: "pkg", IncludeBundle: mirrorv1alpha1.IncludeBundle{MaxVersion: "4.0"}},
				},
			}},
		}
		Expect(mgr.BuildSignature(ops1)).NotTo(Equal(mgr.BuildSignature(ops2)))
	})

	It("includes channel details in signature", func() {
		ops1 := []mirrorv1alpha1.Operator{
			{Catalog: "cat:v1", IncludeConfig: mirrorv1alpha1.IncludeConfig{
				Packages: []mirrorv1alpha1.IncludePackage{
					{Name: "pkg", Channels: []mirrorv1alpha1.IncludeChannel{{Name: "stable"}}},
				},
			}},
		}
		ops2 := []mirrorv1alpha1.Operator{
			{Catalog: "cat:v1", IncludeConfig: mirrorv1alpha1.IncludeConfig{
				Packages: []mirrorv1alpha1.IncludePackage{
					{Name: "pkg", Channels: []mirrorv1alpha1.IncludeChannel{{Name: "fast"}}},
				},
			}},
		}
		Expect(mgr.BuildSignature(ops1)).NotTo(Equal(mgr.BuildSignature(ops2)))
	})

	It("is order-independent for channels", func() {
		ops1 := []mirrorv1alpha1.Operator{
			{Catalog: "cat:v1", IncludeConfig: mirrorv1alpha1.IncludeConfig{
				Packages: []mirrorv1alpha1.IncludePackage{
					{Name: "pkg", Channels: []mirrorv1alpha1.IncludeChannel{
						{Name: "stable"}, {Name: "fast"},
					}},
				},
			}},
		}
		ops2 := []mirrorv1alpha1.Operator{
			{Catalog: "cat:v1", IncludeConfig: mirrorv1alpha1.IncludeConfig{
				Packages: []mirrorv1alpha1.IncludePackage{
					{Name: "pkg", Channels: []mirrorv1alpha1.IncludeChannel{
						{Name: "fast"}, {Name: "stable"},
					}},
				},
			}},
		}
		Expect(mgr.BuildSignature(ops1)).To(Equal(mgr.BuildSignature(ops2)))
	})

	It("includes channel version ranges in signature", func() {
		ops1 := []mirrorv1alpha1.Operator{
			{Catalog: "cat:v1", IncludeConfig: mirrorv1alpha1.IncludeConfig{
				Packages: []mirrorv1alpha1.IncludePackage{
					{Name: "pkg", Channels: []mirrorv1alpha1.IncludeChannel{
						{Name: "stable", IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "1.0", MaxVersion: "2.0"}},
					}},
				},
			}},
		}
		ops2 := []mirrorv1alpha1.Operator{
			{Catalog: "cat:v1", IncludeConfig: mirrorv1alpha1.IncludeConfig{
				Packages: []mirrorv1alpha1.IncludePackage{
					{Name: "pkg", Channels: []mirrorv1alpha1.IncludeChannel{
						{Name: "stable", IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "1.0", MaxVersion: "3.0"}},
					}},
				},
			}},
		}
		Expect(mgr.BuildSignature(ops1)).NotTo(Equal(mgr.BuildSignature(ops2)))
	})
})

// ---------- buildJobSpec ----------

var _ = Describe("buildJobSpec", func() {
	var (
		mgr *CatalogBuildManager
		is  *mirrorv1alpha1.ImageSet
		mt  *mirrorv1alpha1.MirrorTarget
	)

	BeforeEach(func() {
		mgr = newManager("registry.example.com/operator:v1")
		is = defaultImageSet()
		mt = defaultMirrorTarget()
	})

	It("builds a valid Job with correct metadata", func() {
		job := mgr.buildJobSpec("test-job", is, mt, "source:v1", "target:v1", nil)

		Expect(job.Name).To(Equal("test-job"))
		Expect(job.Namespace).To(Equal("test-ns"))
		Expect(job.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "oc-mirror-operator"))
		Expect(job.Labels).To(HaveKeyWithValue("mirror.openshift.io/imageset", "test-is"))
	})

	It("sets owner reference to the ImageSet", func() {
		job := mgr.buildJobSpec("test-job", is, mt, "source:v1", "target:v1", nil)
		Expect(job.OwnerReferences).To(HaveLen(1))
		Expect(job.OwnerReferences[0].Name).To(Equal("test-is"))
		Expect(job.OwnerReferences[0].Kind).To(Equal("ImageSet"))
	})

	It("sets backoff limit and TTL", func() {
		job := mgr.buildJobSpec("test-job", is, mt, "source:v1", "target:v1", nil)
		Expect(*job.Spec.BackoffLimit).To(Equal(int32(3)))
		Expect(*job.Spec.TTLSecondsAfterFinished).To(Equal(int32(600)))
	})

	It("sets correct container spec", func() {
		job := mgr.buildJobSpec("test-job", is, mt, "source:v1", "target:v1", nil)
		containers := job.Spec.Template.Spec.Containers
		Expect(containers).To(HaveLen(1))
		Expect(containers[0].Name).To(Equal("catalog-builder"))
		Expect(containers[0].Image).To(Equal("registry.example.com/operator:v1"))
		Expect(containers[0].Command).To(Equal([]string{"/catalog-builder"}))
		Expect(containers[0].ImagePullPolicy).To(Equal(corev1.PullIfNotPresent))
	})

	It("sets security context", func() {
		job := mgr.buildJobSpec("test-job", is, mt, "source:v1", "target:v1", nil)
		podSec := job.Spec.Template.Spec.SecurityContext
		Expect(*podSec.RunAsNonRoot).To(BeTrue())
		Expect(podSec.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))

		cSec := job.Spec.Template.Spec.Containers[0].SecurityContext
		Expect(*cSec.AllowPrivilegeEscalation).To(BeFalse())
		Expect(cSec.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
	})

	It("sets SOURCE_CATALOG and TARGET_REF env vars", func() {
		job := mgr.buildJobSpec("test-job", is, mt, "quay.io/catalog:v1", "mirror.io/catalog:v1", nil)
		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap).To(HaveKeyWithValue("SOURCE_CATALOG", "quay.io/catalog:v1"))
		Expect(envMap).To(HaveKeyWithValue("TARGET_REF", "mirror.io/catalog:v1"))
	})

	It("sets CATALOG_PACKAGES from package names", func() {
		p := []mirrorv1alpha1.IncludePackage{
			{Name: "web-terminal"},
			{Name: "local-storage"},
		}
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", p)
		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap["CATALOG_PACKAGES"]).To(Equal("web-terminal,local-storage"))
	})

	It("sets CATALOG_INCLUDE_CONFIG as JSON when packages are present", func() {
		p := []mirrorv1alpha1.IncludePackage{
			{Name: "web-terminal", Channels: []mirrorv1alpha1.IncludeChannel{{Name: "stable"}}},
		}
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", p)
		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap).To(HaveKey("CATALOG_INCLUDE_CONFIG"))

		var decoded []mirrorv1alpha1.IncludePackage
		Expect(json.Unmarshal([]byte(envMap["CATALOG_INCLUDE_CONFIG"]), &decoded)).To(Succeed())
		Expect(decoded).To(HaveLen(1))
		Expect(decoded[0].Name).To(Equal("web-terminal"))
	})

	It("does not set CATALOG_INCLUDE_CONFIG when packages are empty", func() {
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)
		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap).NotTo(HaveKey("CATALOG_INCLUDE_CONFIG"))
	})

	It("sets REGISTRY_INSECURE_HOSTS when insecure is true", func() {
		mt.Spec.Insecure = true
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)
		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap).To(HaveKeyWithValue("REGISTRY_INSECURE_HOSTS", "registry.example.com"))
	})

	It("strips path from registry for insecure hosts", func() {
		mt.Spec.Insecure = true
		mt.Spec.Registry = "registry.example.com/deep/path"
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)
		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap["REGISTRY_INSECURE_HOSTS"]).To(Equal("registry.example.com"))
	})

	It("does not set REGISTRY_INSECURE_HOSTS when insecure is false", func() {
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)
		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap).NotTo(HaveKey("REGISTRY_INSECURE_HOSTS"))
	})

	It("mounts auth secret when AuthSecret is set", func() {
		mt.Spec.AuthSecret = "my-pull-secret"
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)

		volNames := volumeNames(job.Spec.Template.Spec.Volumes)
		Expect(volNames).To(ContainElement("registry-auth"))

		mounts := mountPathMap(job.Spec.Template.Spec.Containers[0].VolumeMounts)
		Expect(mounts).To(HaveKeyWithValue("registry-auth", "/var/run/secrets/registry"))

		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap).To(HaveKeyWithValue("DOCKER_CONFIG", "/var/run/secrets/registry"))
	})

	It("does not mount auth volume when AuthSecret is empty", func() {
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)
		volNames := volumeNames(job.Spec.Template.Spec.Volumes)
		Expect(volNames).NotTo(ContainElement("registry-auth"))
	})

	It("always includes blob-buffer volume", func() {
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)
		volNames := volumeNames(job.Spec.Template.Spec.Volumes)
		Expect(volNames).To(ContainElement("blob-buffer"))
		mounts := mountPathMap(job.Spec.Template.Spec.Containers[0].VolumeMounts)
		Expect(mounts).To(HaveKeyWithValue("blob-buffer", "/tmp/blob-buffer"))
	})

	It("injects proxy env vars when configured", func() {
		mt.Spec.Proxy = &mirrorv1alpha1.ProxyConfig{
			HTTPProxy:  "http://proxy:3128",
			HTTPSProxy: "https://proxy:3129",
			NoProxy:    "10.0.0.0/8",
		}
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)
		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap).To(HaveKey("HTTP_PROXY"))
		Expect(envMap).To(HaveKey("HTTPS_PROXY"))
		Expect(envMap).To(HaveKey("NO_PROXY"))
	})

	It("mounts CA bundle when configured with custom key", func() {
		mt.Spec.CABundle = &mirrorv1alpha1.CABundleRef{
			ConfigMapName: "my-ca-bundle",
			Key:           "custom-ca.crt",
		}
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)

		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap).To(HaveKeyWithValue("SSL_CERT_FILE", "/run/secrets/ca/custom-ca.crt"))

		volNames := volumeNames(job.Spec.Template.Spec.Volumes)
		Expect(volNames).To(ContainElement("ca-bundle"))

		mounts := mountPathMap(job.Spec.Template.Spec.Containers[0].VolumeMounts)
		Expect(mounts).To(HaveKeyWithValue("ca-bundle", "/run/secrets/ca"))
	})

	It("defaults CA bundle key to ca-bundle.crt", func() {
		mt.Spec.CABundle = &mirrorv1alpha1.CABundleRef{
			ConfigMapName: "my-ca-bundle",
		}
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)
		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap).To(HaveKeyWithValue("SSL_CERT_FILE", "/run/secrets/ca/ca-bundle.crt"))
	})

	It("propagates node selector and tolerations", func() {
		mt.Spec.Worker.NodeSelector = map[string]string{"node-role": "infra"}
		mt.Spec.Worker.Tolerations = []corev1.Toleration{
			{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "infra", Effect: corev1.TaintEffectNoSchedule},
		}
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)
		Expect(job.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("node-role", "infra"))
		Expect(job.Spec.Template.Spec.Tolerations).To(HaveLen(1))
	})

	It("propagates worker resource requirements", func() {
		mt.Spec.Worker.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		}
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)
		res := job.Spec.Template.Spec.Containers[0].Resources
		Expect(res.Requests.Cpu().String()).To(Equal("500m"))
		Expect(res.Requests.Memory().String()).To(Equal("256Mi"))
	})

	It("sets restart policy to Never", func() {
		job := mgr.buildJobSpec("test-job", is, mt, "src:v1", "tgt:v1", nil)
		Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
	})
})

// ---------- EnsureCatalogBuildJob ----------

var _ = Describe("EnsureCatalogBuildJob", func() {
	var (
		ctx context.Context
		mgr *CatalogBuildManager
		is  *mirrorv1alpha1.ImageSet
		mt  *mirrorv1alpha1.MirrorTarget
	)

	BeforeEach(func() {
		ctx = context.Background()
		mgr = newManager("registry.example.com/operator:v1")
		is = defaultImageSet()
		mt = defaultMirrorTarget()
	})

	It("creates a new job when none exists", func() {
		c := newFakeClient()
		err := mgr.EnsureCatalogBuildJob(ctx, c, is, mt, "quay.io/catalog:v1", "mirror/catalog:v1", nil)
		Expect(err).NotTo(HaveOccurred())

		name := JobName(is.Name, "quay.io/catalog:v1")
		job := &batchv1.Job{}
		Expect(c.Get(ctx, client.ObjectKey{Name: name, Namespace: "test-ns"}, job)).To(Succeed())
		Expect(job.Name).To(Equal(name))
	})

	It("is a no-op when a job already exists", func() {
		c := newFakeClient()

		err := mgr.EnsureCatalogBuildJob(ctx, c, is, mt, "quay.io/catalog:v1", "mirror/catalog:v1", nil)
		Expect(err).NotTo(HaveOccurred())

		err = mgr.EnsureCatalogBuildJob(ctx, c, is, mt, "quay.io/catalog:v1", "mirror/catalog:v1", nil)
		Expect(err).NotTo(HaveOccurred())
	})

	It("passes packages to the job", func() {
		c := newFakeClient()
		p := []mirrorv1alpha1.IncludePackage{
			{Name: "web-terminal"},
			{Name: "local-storage"},
		}
		err := mgr.EnsureCatalogBuildJob(ctx, c, is, mt, "quay.io/catalog:v1", "mirror/catalog:v1", p)
		Expect(err).NotTo(HaveOccurred())

		name := JobName(is.Name, "quay.io/catalog:v1")
		job := &batchv1.Job{}
		Expect(c.Get(ctx, client.ObjectKey{Name: name, Namespace: "test-ns"}, job)).To(Succeed())
		envMap := envToMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(envMap["CATALOG_PACKAGES"]).To(Equal("web-terminal,local-storage"))
	})
})

// ---------- GetBuildJobStatus ----------

var _ = Describe("GetBuildJobStatus", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("returns NotFound when job does not exist", func() {
		c := newFakeClient()
		phase, err := GetBuildJobStatus(ctx, c, "nonexistent-job", "test-ns")
		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal(JobPhaseNotFound))
	})

	It("returns Succeeded when job has succeeded", func() {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "test-ns"},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}
		c := newFakeClient(job)
		phase, err := GetBuildJobStatus(ctx, c, "test-job", "test-ns")
		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal(JobPhaseSucceeded))
	})

	It("returns Failed when job has failed", func() {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "test-ns"},
			Status:     batchv1.JobStatus{Failed: 1},
		}
		c := newFakeClient(job)
		phase, err := GetBuildJobStatus(ctx, c, "test-job", "test-ns")
		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal(JobPhaseFailed))
	})

	It("returns Running when job has active pods", func() {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "test-ns"},
			Status:     batchv1.JobStatus{Active: 1},
		}
		c := newFakeClient(job)
		phase, err := GetBuildJobStatus(ctx, c, "test-job", "test-ns")
		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal(JobPhaseRunning))
	})

	It("returns Pending when job has no status", func() {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "test-ns"},
			Status:     batchv1.JobStatus{},
		}
		c := newFakeClient(job)
		phase, err := GetBuildJobStatus(ctx, c, "test-job", "test-ns")
		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal(JobPhasePending))
	})

	It("prioritizes Succeeded over Failed", func() {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "test-ns"},
			Status:     batchv1.JobStatus{Succeeded: 1, Failed: 1},
		}
		c := newFakeClient(job)
		phase, err := GetBuildJobStatus(ctx, c, "test-job", "test-ns")
		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal(JobPhaseSucceeded))
	})
})

// ---------- DeleteBuildJob ----------

var _ = Describe("DeleteBuildJob", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("succeeds when job does not exist", func() {
		c := newFakeClient()
		err := DeleteBuildJob(ctx, c, "nonexistent", "test-ns")
		Expect(err).NotTo(HaveOccurred())
	})

	It("deletes an existing job", func() {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "test-ns"},
		}
		c := newFakeClient(job)
		err := DeleteBuildJob(ctx, c, "test-job", "test-ns")
		Expect(err).NotTo(HaveOccurred())

		phase, err := GetBuildJobStatus(ctx, c, "test-job", "test-ns")
		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal(JobPhaseNotFound))
	})
})

// ---------- New / MustNew ----------

var _ = Describe("MustNew", func() {
	It("panics when OPERATOR_IMAGE is not set", func() {
		GinkgoT().Setenv(OperatorImageEnvVar, "")
		Expect(func() { MustNew() }).To(Panic())
	})

	It("returns a manager when OPERATOR_IMAGE is set", func() {
		GinkgoT().Setenv(OperatorImageEnvVar, "registry.example.com/operator:v1")
		var m *CatalogBuildManager
		Expect(func() { m = MustNew() }).NotTo(Panic())
		Expect(m).NotTo(BeNil())
	})
})

// ---------- helpers ----------

func envToMap(envs []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(envs))
	for _, e := range envs {
		m[e.Name] = e.Value
	}
	return m
}

func volumeNames(vols []corev1.Volume) []string {
	names := make([]string, 0, len(vols))
	for _, v := range vols {
		names = append(names, v.Name)
	}
	return names
}

func mountPathMap(mounts []corev1.VolumeMount) map[string]string {
	m := make(map[string]string, len(mounts))
	for _, vm := range mounts {
		m[vm.Name] = vm.MountPath
	}
	return m
}
