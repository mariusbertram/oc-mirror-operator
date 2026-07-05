/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package e2e: cluster-level end-to-end test for spec.mirror.helm.repositories —
// verifies a container image referenced by a Helm chart's rendered templates
// gets mirrored, against a real cluster + registry. The Helm repository itself
// is a small self-hosted static file server inside the Kind cluster (an nginx
// pod serving a synthetic index.yaml + chart archive built by this test), so
// the test has no dependency on any real, external Helm repository.
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mariusbertram/oc-mirror-operator/test/utils"
)

var _ = Describe("Helm Chart Mirroring E2E", Ordered, Label("cluster"), func() {
	const (
		ns             = operatorNamespace
		targetName     = "helm-mirror-target"
		imageSetName   = "helm-mirror-imageset"
		registryHost   = "registry.default.svc.cluster.local:5000"
		helmRepoHost   = "helm-repo.default.svc.cluster.local:80"
		chartImageRef  = "docker.io/library/redis:7-alpine"
		chartImageName = "redis"
	)

	BeforeAll(func() {
		By("deploying the in-cluster registry")
		cmd := exec.Command("kubectl", "apply", "-f", "config/samples/registry_deploy.yaml")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy in-cluster registry")

		By("waiting for the registry to be ready")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/registry",
			"-n", "default", "--timeout=120s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Registry deployment did not become ready")

		By("building a synthetic Helm chart repository (index.yaml + chart archive)")
		tmpDir, err := os.MkdirTemp("", "helm-repo-e2e-*")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = os.RemoveAll(tmpDir) })

		archivePath := filepath.Join(tmpDir, "mychart-1.0.0.tgz")
		Expect(os.WriteFile(archivePath, buildHelmTestChartArchive(chartImageRef), 0644)).To(Succeed())

		indexPath := filepath.Join(tmpDir, "index.yaml")
		indexYAML := `apiVersion: v1
entries:
  mychart:
    - name: mychart
      version: 1.0.0
      urls:
        - mychart-1.0.0.tgz
`
		Expect(os.WriteFile(indexPath, []byte(indexYAML), 0644)).To(Succeed())

		By("creating the ConfigMap backing the Helm repository content")
		genCM := exec.Command("kubectl", "create", "configmap", "helm-repo-content",
			"-n", "default",
			"--from-file=index.yaml="+indexPath,
			"--from-file=mychart-1.0.0.tgz="+archivePath,
			"--dry-run=client", "-o", "yaml")
		cmYAML, err := utils.Run(genCM)
		Expect(err).NotTo(HaveOccurred(), "Failed to render helm-repo-content ConfigMap")
		applyCM := exec.Command("kubectl", "apply", "-f", "-")
		applyCM.Stdin = strings.NewReader(cmYAML)
		_, err = utils.Run(applyCM)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply helm-repo-content ConfigMap")

		By("deploying the static Helm repository server")
		helmRepoYAML := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helm-repo
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: helm-repo
  template:
    metadata:
      labels:
        app: helm-repo
    spec:
      containers:
      - name: helm-repo
        image: docker.io/library/nginx:1.27-alpine
        ports:
        - containerPort: 80
        volumeMounts:
        - name: content
          mountPath: /usr/share/nginx/html
      volumes:
      - name: content
        configMap:
          name: helm-repo-content
---
apiVersion: v1
kind: Service
metadata:
  name: helm-repo
  namespace: default
spec:
  ports:
  - port: 80
    targetPort: 80
  selector:
    app: helm-repo
`
		applyRepo := exec.Command("kubectl", "apply", "-f", "-")
		applyRepo.Stdin = strings.NewReader(helmRepoYAML)
		_, err = utils.Run(applyRepo)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy Helm repository server")

		By("waiting for the Helm repository server to be ready")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/helm-repo",
			"-n", "default", "--timeout=120s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Helm repository server did not become ready")
	})

	AfterAll(func() {
		By("removing finalizers from test resources")
		_ = exec.Command("kubectl", "patch", "imageset", imageSetName,
			"-n", ns, "-p", `{"metadata":{"finalizers":[]}}`, "--type=merge").Run()
		_ = exec.Command("kubectl", "patch", "mirrortarget", targetName,
			"-n", ns, "-p", `{"metadata":{"finalizers":[]}}`, "--type=merge").Run()

		By("deleting test resources and waiting for removal")
		_ = exec.Command("kubectl", "delete", "imageset", imageSetName,
			"-n", ns, "--ignore-not-found=true", "--timeout=60s").Run()
		_ = exec.Command("kubectl", "delete", "mirrortarget", targetName,
			"-n", ns, "--ignore-not-found=true", "--timeout=60s").Run()
		_ = exec.Command("kubectl", "delete", "deployment", "helm-repo",
			"-n", "default", "--ignore-not-found=true", "--timeout=60s").Run()
		_ = exec.Command("kubectl", "delete", "service", "helm-repo",
			"-n", "default", "--ignore-not-found=true", "--timeout=60s").Run()
		_ = exec.Command("kubectl", "delete", "configmap", "helm-repo-content",
			"-n", "default", "--ignore-not-found=true", "--timeout=60s").Run()
		_ = exec.Command("kubectl", "delete", "-f",
			"config/samples/registry_deploy.yaml", "--ignore-not-found=true", "--timeout=60s").Run()
	})

	Context("helm.repositories mirrors images referenced by a chart's rendered templates", func() {
		It("should mirror the image found in the chart's Deployment template", func() {
			By("creating the MirrorTarget")
			mtYAML := fmt.Sprintf(`
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: %s
  namespace: %s
spec:
  registry: %s/mirror
  insecure: true
  imageSets:
    - %s
`, targetName, ns, registryHost, imageSetName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(mtYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create MirrorTarget")

			By("creating the ImageSet with a helm repository entry")
			isYAML := fmt.Sprintf(`
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: %s
  namespace: %s
spec:
  mirror:
    helm:
      repositories:
        - name: test-repo
          url: http://%s
          charts:
            - name: mychart
              version: 1.0.0
`, imageSetName, ns, helmRepoHost)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(isYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ImageSet")

			By("verifying the chart's image gets resolved and mirrored")
			verifyMirroring := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "imageset", imageSetName,
					"-n", ns,
					"-o", "jsonpath={.status.totalImages}:{.status.mirroredImages}:{.status.pendingImages}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				parts := strings.SplitN(strings.TrimSpace(output), ":", 3)
				g.Expect(parts).To(HaveLen(3), fmt.Sprintf("unexpected status output: %q", output))
				total, mirrored, pending := parts[0], parts[1], parts[2]
				g.Expect(total).NotTo(BeEmpty())
				g.Expect(total).NotTo(Equal("0"), "no images resolved from the Helm chart yet")
				g.Expect(pending).To(Or(Equal("0"), BeEmpty()),
					"images still pending — total=%s mirrored=%s pending=%s", total, mirrored, pending)
				g.Expect(mirrored).To(Equal(total),
					"not all images mirrored — total=%s mirrored=%s", total, mirrored)
			}
			Eventually(verifyMirroring, 8*time.Minute, 10*time.Second).Should(Succeed(), func() string {
				return mirrorDiagnosticDump(ns, imageSetName)
			})

			By("verifying the chart's image is referenced in the generated IDMS")
			cmd = exec.Command("kubectl", "get", "configmap",
				fmt.Sprintf("oc-mirror-%s-resources", targetName),
				"-n", ns,
				"-o", "jsonpath={.data.idms\\.yaml}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring(chartImageName),
				"IDMS should reference the Helm chart's image: %s", output)
		})
	})
})

// buildHelmTestChartArchive builds a minimal, valid Helm chart .tgz named
// "mychart" version "1.0.0", with a single Deployment template referencing
// imageRef at the default image JSONPath the resolver scans.
func buildHelmTestChartArchive(imageRef string) []byte {
	const name = "mychart"
	const version = "1.0.0"
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	deployment := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  template:
    spec:
      containers:
        - name: myapp
          image: %s
`, imageRef)

	files := map[string]string{
		name + "/Chart.yaml":                "apiVersion: v2\nname: " + name + "\nversion: " + version + "\n",
		name + "/templates/deployment.yaml": deployment,
	}
	for path, content := range files {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     path,
			Size:     int64(len(content)),
			Mode:     0644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			panic(fmt.Sprintf("write tar header for %s: %v", path, err))
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			panic(fmt.Sprintf("write tar content for %s: %v", path, err))
		}
	}
	if err := tw.Close(); err != nil {
		panic(fmt.Sprintf("close tar writer: %v", err))
	}
	if err := gzw.Close(); err != nil {
		panic(fmt.Sprintf("close gzip writer: %v", err))
	}
	return buf.Bytes()
}
