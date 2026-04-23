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
	"context"
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
		mirrorNamespace   = "default"
		operatorNamespace = "oc-mirror-system"
		targetName        = "internal-registry"
		imageSetName      = "test-sync-e2e"
	)

	BeforeAll(func() {
		By("deploying the local registry in the cluster")
		cmd := exec.Command("kubectl", "apply", "-f", "config/samples/registry_deploy.yaml")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy local registry")

		By("waiting for registry to be ready")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/registry", "--timeout=60s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Registry failed to become ready")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		// Ensure CRDs are fully established before proceeding.
		By("waiting for CRDs to be established")
		cmd = exec.Command("kubectl", "wait", "--for=condition=Established",
			"crd/imagesets.mirror.openshift.io",
			"crd/mirrortargets.mirror.openshift.io",
			"--timeout=60s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "CRDs did not become Established")

		By("deploying the controller-manager")
		// Using the projectImage built in BeforeSuite
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("waiting for controller-manager to be ready")
		verifyControllerUp := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager", "-n", operatorNamespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("Running"))
		}
		Eventually(verifyControllerUp, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		// Remove finalizers first so the operator is not required to be running for deletion.
		// This prevents CRDs from getting stuck in "terminating" (which would block the next
		// test suite's BeforeAll when it tries to re-install CRDs via `make install`).
		By("removing finalizers from test resources")
		patchCtx, patchCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer patchCancel()
		_ = exec.CommandContext(patchCtx, "kubectl", "patch", "imageset", imageSetName,
			"-n", mirrorNamespace, "-p", `{"metadata":{"finalizers":[]}}`, "--type=merge").Run()
		_ = exec.CommandContext(patchCtx, "kubectl", "patch", "mirrortarget", targetName,
			"-n", mirrorNamespace, "-p", `{"metadata":{"finalizers":[]}}`, "--type=merge").Run()

		By("deleting test resources")
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer deleteCancel()
		_ = exec.CommandContext(deleteCtx, "kubectl", "delete", "imageset", imageSetName,
			"-n", mirrorNamespace, "--ignore-not-found=true").Run()
		_ = exec.CommandContext(deleteCtx, "kubectl", "delete", "mirrortarget", targetName,
			"-n", mirrorNamespace, "--ignore-not-found=true").Run()
		_ = exec.CommandContext(deleteCtx, "kubectl", "delete", "-f",
			"config/samples/registry_deploy.yaml", "--ignore-not-found=true").Run()

		By("undeploying the operator")
		undeployCtx, undeployCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer undeployCancel()
		_ = exec.CommandContext(undeployCtx, "make", "undeploy").Run()
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
			Eventually(verifyMirroring, 5*time.Minute, 10*time.Second).Should(Succeed())

		})
	})
})
