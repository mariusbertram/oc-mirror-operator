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

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mariusbertram/oc-mirror-operator/test/utils"
)

var _ = Describe("oc-mirror Operator E2E", Ordered, Label("cluster"), func() {
	const (
		mirrorNamespace = "default"
		targetName      = "internal-registry"
		imageSetName    = "test-sync-e2e"
	)

	BeforeAll(func() {
		By("deploying the local registry in the cluster")
		cmd := exec.Command("kubectl", "apply", "-f", "config/samples/registry_deploy.yaml")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy local registry")

		By("waiting for registry to be ready")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/registry", "--timeout=120s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Registry failed to become ready")
	})

	AfterAll(func() {
		By("removing finalizers from test resources")
		_ = exec.Command("kubectl", "patch", "imageset", imageSetName,
			"-n", mirrorNamespace, "-p", `{"metadata":{"finalizers":[]}}`, "--type=merge").Run()
		_ = exec.Command("kubectl", "patch", "mirrortarget", targetName,
			"-n", mirrorNamespace, "-p", `{"metadata":{"finalizers":[]}}`, "--type=merge").Run()

		By("deleting test resources and waiting for removal")
		_ = exec.Command("kubectl", "delete", "imageset", imageSetName,
			"-n", mirrorNamespace, "--ignore-not-found=true", "--timeout=60s").Run()
		_ = exec.Command("kubectl", "delete", "mirrortarget", targetName,
			"-n", mirrorNamespace, "--ignore-not-found=true", "--timeout=60s").Run()
		_ = exec.Command("kubectl", "delete", "-f",
			"config/samples/registry_deploy.yaml", "--ignore-not-found=true", "--timeout=60s").Run()
	})

	Context("Mirroring Scenario", func() {
		It("should mirror an image to the local registry", func() {
			By("creating the MirrorTarget")
			mirrorTargetYaml := fmt.Sprintf(`
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: %s
spec:
  registry: registry.default.svc.cluster.local:5000/mirror
  insecure: true
  imageSets:
    - %s
`, targetName, imageSetName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(mirrorTargetYaml)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating the ImageSet")
			imageSetYaml := fmt.Sprintf(`
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: %s
spec:
  mirror:
    additionalImages:
      - name: docker.io/library/alpine:latest
`, imageSetName)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(imageSetYaml)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the ImageSet status becomes Mirrored")
			verifyMirroring := func(g Gomega) {
				// The Manager pod sets totalImages, mirroredImages, and pendingImages.
				// Poll until all images are mirrored (pending == 0, mirrored == total > 0).
				// Note: integer fields with omitempty are absent from JSON when 0, so
				// jsonpath returns "" instead of "0" — treat "" and "0" as equivalent.
				cmd := exec.Command("kubectl", "get", "imageset", imageSetName,
					"-o", "jsonpath={.status.totalImages}:{.status.mirroredImages}:{.status.pendingImages}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				parts := strings.SplitN(strings.TrimSpace(output), ":", 3)
				g.Expect(parts).To(HaveLen(3), fmt.Sprintf("unexpected status output: %q", output))
				total, mirrored, pending := parts[0], parts[1], parts[2]
				g.Expect(total).NotTo(BeEmpty())
				g.Expect(total).NotTo(Equal("0"), "no images resolved yet")
				// pending="" means 0 (omitempty); accept both forms
				g.Expect(pending).To(Or(Equal("0"), BeEmpty()), fmt.Sprintf(
					"images still pending — total=%s mirrored=%s pending=%s", total, mirrored, pending))
				g.Expect(mirrored).To(Equal(total), fmt.Sprintf(
					"not all images mirrored — total=%s mirrored=%s", total, mirrored))
			}
			// Give the manager pod time to resolve and mirror the image.
			Eventually(verifyMirroring, 8*time.Minute, 10*time.Second).Should(Succeed(), func() string {
				return mirrorDiagnosticDump(mirrorNamespace, imageSetName)
			})
		})

		It("should create the Resource API Deployment and Service", func() {
			By("verifying the oc-mirror-resource-api Deployment exists and is ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "oc-mirror-resource-api",
					"-n", mirrorNamespace,
					"-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"), "Resource API Deployment not ready (readyReplicas=%s)", output)
			}, 3*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the per-target Resource Service exists on port 8081")
			cmd := exec.Command("kubectl", "get", "service", targetName+"-resources",
				"-n", mirrorNamespace,
				"-o", "jsonpath={.spec.ports[0].port}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("8081"), "Resource Service port mismatch: %s", output)
		})

		It("should persist generated resources in a ConfigMap", func() {
			By("verifying the Resource ConfigMap exists with expected keys")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap",
					fmt.Sprintf("oc-mirror-%s-resources", targetName),
					"-n", mirrorNamespace,
					"-o", "jsonpath={.data}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("index.json"),
					"ConfigMap missing index.json key")
				g.Expect(output).To(ContainSubstring("idms.yaml"),
					"ConfigMap missing idms.yaml key")
			}, 3*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying IDMS content contains ImageDigestMirrorSet")
			cmd := exec.Command("kubectl", "get", "configmap",
				fmt.Sprintf("oc-mirror-%s-resources", targetName),
				"-n", mirrorNamespace,
				"-o", fmt.Sprintf("jsonpath={.data.%s}", imageSetName+"-idms\\.yaml"))
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("ImageDigestMirrorSet"),
				"IDMS YAML does not contain ImageDigestMirrorSet")
		})
	})
})

// mirrorDiagnosticDump collects diagnostic info when mirror tests fail.
func mirrorDiagnosticDump(namespace, imageSetName string) string {
	var diag strings.Builder
	diag.WriteString("\n=== Mirror test diagnostic dump ===\n")

	diagCmds := []struct {
		label string
		args  []string
	}{
		{"ImageSet YAML", []string{"get", "imageset", imageSetName, "-n", namespace, "-o", "yaml"}},
		{"Pods", []string{"get", "pods", "-n", namespace, "-o", "wide"}},
		{"Manager logs (last 80 lines)", []string{"logs", "-l", "app=oc-mirror-manager", "-n", namespace, "--tail=80", "--all-containers"}},
		{"Resource API logs (last 40 lines)", []string{"logs", "-l", "app=oc-mirror-resource-api", "-n", namespace, "--tail=40", "--all-containers"}},
		{"Events", []string{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"}},
	}
	for _, dc := range diagCmds {
		out, err := exec.Command("kubectl", dc.args...).CombinedOutput()
		fmt.Fprintf(&diag, "\n--- %s ---\n", dc.label)
		if err != nil {
			fmt.Fprintf(&diag, "ERROR: %v\n%s\n", err, string(out))
		} else {
			fmt.Fprintf(&diag, "%s\n", string(out))
		}
	}
	return diag.String()
}
