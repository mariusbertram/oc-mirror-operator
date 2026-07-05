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

// Package e2e: cluster-level end-to-end test for spec.mirror.platform.graph —
// verifies the manager pod builds and pushes the Cincinnati graph-data image
// against a real cluster + registry. Requires outbound access to
// api.openshift.com and registry.access.redhat.com from the manager pod
// (same egress already relied on by the alpine/catalog cluster tests pulling
// from docker.io/ghcr.io).
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

var _ = Describe("Cincinnati Graph-Data Image E2E", Ordered, Label("cluster"), func() {
	const (
		ns           = operatorNamespace
		targetName   = "graph-data-target"
		imageSetName = "graph-data-imageset"
		registryHost = "registry.default.svc.cluster.local:5000"
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

	Context("platform.graph builds and pushes the OSUS graph-data image", func() {
		It("should set the graph-image-built-at annotation once the image is pushed", func() {
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

			By("creating the ImageSet with platform.graph enabled")
			isYAML := fmt.Sprintf(`
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: %s
  namespace: %s
spec:
  mirror:
    platform:
      graph: true
`, imageSetName, ns)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(isYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ImageSet")

			By("verifying the mirror.openshift.io/graph-image-built-at annotation appears within 5 minutes")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "imageset", imageSetName,
					"-n", ns,
					"-o", `jsonpath={.metadata.annotations.mirror\.openshift\.io/graph-image-built-at}`)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).NotTo(BeEmpty(),
					"graph-image-built-at annotation not set yet — graph image build/push likely failed or is still running")
			}, 5*time.Minute, 10*time.Second).Should(Succeed(), func() string {
				return graphDiagnosticDump(ns, imageSetName)
			})
		})
	})
})

// graphDiagnosticDump collects diagnostic info when the graph-data test fails.
func graphDiagnosticDump(namespace, imageSetName string) string {
	var diag strings.Builder
	diag.WriteString("\n=== Graph-data image test diagnostic dump ===\n")

	diagCmds := []struct {
		label string
		args  []string
	}{
		{"ImageSet YAML", []string{"get", "imageset", imageSetName, "-n", namespace, "-o", "yaml"}},
		{"Manager logs (last 100 lines)", []string{"logs", "-l", "app=oc-mirror-manager", "-n", namespace, "--tail=100", "--all-containers"}},
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
