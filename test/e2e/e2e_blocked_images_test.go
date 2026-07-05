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

// Package e2e: cluster-level end-to-end test for spec.mirror.blockedImages —
// verifies a blocked additionalImage is excluded from mirroring while its
// sibling still gets mirrored normally, against a real cluster + registry.
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

var _ = Describe("Blocked Images E2E", Ordered, Label("cluster"), func() {
	const (
		ns           = operatorNamespace
		targetName   = "blocked-images-target"
		imageSetName = "blocked-images-imageset"
		registryHost = "registry.default.svc.cluster.local:5000"
		blockedImage = "docker.io/library/busybox"
		allowedImage = "docker.io/library/alpine:latest"
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
		_ = exec.Command("kubectl", "delete", "-f",
			"config/samples/registry_deploy.yaml", "--ignore-not-found=true", "--timeout=60s").Run()
	})

	Context("blockedImages excludes a configured image from mirroring", func() {
		It("should mirror only the non-blocked additional image", func() {
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

			By("creating the ImageSet with two additionalImages, one of them blocked")
			isYAML := fmt.Sprintf(`
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: %s
  namespace: %s
spec:
  mirror:
    additionalImages:
      - name: %s
      - name: %s:latest
    blockedImages:
      - name: %s
`, imageSetName, ns, allowedImage, blockedImage, blockedImage)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(isYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ImageSet")

			By("verifying only the non-blocked image is counted and mirrored")
			verifyMirroring := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "imageset", imageSetName,
					"-n", ns,
					"-o", "jsonpath={.status.totalImages}:{.status.mirroredImages}:{.status.pendingImages}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				parts := strings.SplitN(strings.TrimSpace(output), ":", 3)
				g.Expect(parts).To(HaveLen(3), fmt.Sprintf("unexpected status output: %q", output))
				total, mirrored, pending := parts[0], parts[1], parts[2]
				g.Expect(total).To(Equal("1"),
					"expected only the non-blocked image to be counted (total=%s)", total)
				g.Expect(pending).To(Or(Equal("0"), BeEmpty()),
					"images still pending — total=%s mirrored=%s pending=%s", total, mirrored, pending)
				g.Expect(mirrored).To(Equal(total),
					"not all images mirrored — total=%s mirrored=%s", total, mirrored)
			}
			Eventually(verifyMirroring, 8*time.Minute, 10*time.Second).Should(Succeed(), func() string {
				return mirrorDiagnosticDump(ns, imageSetName)
			})

			By("verifying the blocked image is absent from the generated IDMS")
			cmd = exec.Command("kubectl", "get", "configmap",
				fmt.Sprintf("oc-mirror-%s-resources", targetName),
				"-n", ns,
				"-o", "jsonpath={.data.idms\\.yaml}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).NotTo(ContainSubstring("busybox"),
				"IDMS should not reference the blocked image: %s", output)
		})
	})
})
