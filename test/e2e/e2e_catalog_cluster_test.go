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

// Package e2e contains cluster-level end-to-end tests for the catalog image
// build pipeline.  These tests require a running Kubernetes cluster (Kind) with
// the operator deployed and a local OCI registry available inside the cluster.
//
// Run with:
//
//	make test-e2e-cluster IMG=example.com/oc-mirror:v0.0.1
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

// Catalog cluster tests use the brtrm-dev-catalog (small, GHCR-hosted, no auth needed)
// instead of the large quay.io/operatorhubio/catalog (~2 GB). They run in standard CI.
var _ = Describe("Catalog Build Job E2E", Ordered, Label("cluster", "catalog-cluster"), func() {
	const (
		ns             = "default"
		targetName     = "catalog-test-target"
		imageSetName   = "catalog-test-imageset"
		sourceCatalog  = "ghcr.io/mariusbertram/brtrm-dev-catalog/catalog:latest"
		catalogPackage = "ip-rule-operator"
		registryHost   = "registry.default.svc.cluster.local:5000"
	)

	BeforeAll(func() {
		By("deploying the in-cluster registry")
		cmd := exec.Command("kubectl", "apply", "-f", "config/samples/registry_deploy.yaml")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy in-cluster registry")

		By("waiting for the registry to be ready")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/registry",
			"-n", ns, "--timeout=120s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Registry deployment did not become ready")
	})

	AfterAll(func() {
		By("removing finalizers from test resources")
		_ = exec.Command("kubectl", "patch", "imageset", imageSetName, "-n", ns,
			"-p", `{"metadata":{"finalizers":[]}}`, "--type=merge").Run()
		_ = exec.Command("kubectl", "patch", "mirrortarget", targetName, "-n", ns,
			"-p", `{"metadata":{"finalizers":[]}}`, "--type=merge").Run()

		By("deleting test resources and waiting for removal")
		_ = exec.Command("kubectl", "delete", "imageset", imageSetName, "-n", ns,
			"--ignore-not-found=true", "--timeout=60s").Run()
		_ = exec.Command("kubectl", "delete", "mirrortarget", targetName, "-n", ns,
			"--ignore-not-found=true", "--timeout=60s").Run()
		_ = exec.Command("kubectl", "delete", "-f", "config/samples/registry_deploy.yaml",
			"--ignore-not-found=true", "--timeout=60s").Run()
	})

	Context("CatalogBuildJob lifecycle", func() {
		It("should create a CatalogBuildJob when an ImageSet specifies an operator catalog", func() {
			By("creating the MirrorTarget with the ImageSet in its list")
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

			By("creating the ImageSet with an operator catalog entry and recollect annotation")
			// The recollect annotation bypasses the "wait for operator images mirrored"
			// gate in imageset_controller, so the CatalogBuildJob is created immediately.
			// This keeps the test focused on catalog image resolution, not image mirroring
			// (mirroring is already covered by the alpine e2e test).
			isYAML := fmt.Sprintf(`
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: %s
  namespace: %s
  annotations:
    mirror.openshift.io/recollect: "true"
spec:
  mirror:
    operators:
      - catalog: %s
        packages:
          - name: %s
`, imageSetName, ns, sourceCatalog, catalogPackage)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(isYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ImageSet")

			By("verifying a CatalogBuildJob is created within 2 minutes")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", "mirror.openshift.io/imageset="+imageSetName,
					"-n", ns,
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(),
					"no CatalogBuildJob found for ImageSet %s", imageSetName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should complete the CatalogBuildJob successfully", func() {
			By("waiting up to 10 minutes for the CatalogBuildJob to succeed")
			// The job pulls ghcr.io/mariusbertram/brtrm-dev-catalog/catalog:latest (small,
			// only 2 operators) and filters it — should complete well within 5 minutes.
			// Extra time accounts for image pull on cold GitHub Actions runners.
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", "mirror.openshift.io/imageset="+imageSetName,
					"-n", ns,
					"-o", "jsonpath={.items[0].status.succeeded}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"),
					"CatalogBuildJob has not succeeded yet (succeeded=%s)", output)
			}, 15*time.Minute, 10*time.Second).Should(Succeed())

			// If the Eventually above fails, the AfterEach/cleanup still runs but
			// we won't reach this point — diagnostics are handled by DeferCleanup below.
		})

		// Dump diagnostic info when the catalog-build test fails.  Using
		// DeferCleanup ensures the output appears in Ginkgo's captured writer
		// output section regardless of which It block failed.
		AfterEach(func() {
			if !CurrentSpecReport().Failed() {
				return
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== CatalogBuildJob diagnostic dump ===\n")

			diagCmds := []struct {
				label string
				args  []string
			}{
				{"Job YAML", []string{"get", "jobs", "-l", "mirror.openshift.io/imageset=" + imageSetName, "-n", ns, "-o", "yaml"}},
				{"Pods", []string{"get", "pods", "-l", "mirror.openshift.io/imageset=" + imageSetName, "-n", ns, "-o", "wide"}},
				{"Pod logs (last 100 lines)", []string{"logs", "-l", "mirror.openshift.io/imageset=" + imageSetName, "-n", ns, "--tail=100", "--all-containers"}},
				{"Events", []string{"get", "events", "-n", ns, "--sort-by=.lastTimestamp", "--field-selector", "reason!=Pulling"}},
				{"Operator controller-manager logs", []string{"logs", "-l", "control-plane=controller-manager", "-n", "oc-mirror-operator-system", "--tail=100", "--all-containers"}},
			}
			for _, dc := range diagCmds {
				out, err := exec.Command("kubectl", dc.args...).CombinedOutput()
				_, _ = fmt.Fprintf(GinkgoWriter, "\n--- %s ---\n", dc.label)
				if err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "ERROR: %v\n%s\n", err, string(out))
				} else {
					_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", string(out))
				}
			}
		})

		It("should set the CatalogReady condition to True on the ImageSet", func() {
			By("checking the CatalogReady condition on the ImageSet")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "imageset", imageSetName,
					"-n", ns,
					"-o", `jsonpath={.status.conditions[?(@.type=="CatalogReady")].status}`)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"),
					"CatalogReady condition is not True (got %q)", output)
			}, 2*time.Minute, 10*time.Second).Should(Succeed())
		})
	})
})
