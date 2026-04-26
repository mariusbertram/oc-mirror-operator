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

// Package e2e contains OLM upgrade tests that verify the operator can be
// upgraded via OLM without RBAC anti-escalation errors.
//
// The test installs the previous release bundle via operator-sdk, then
// upgrades to a locally-built bundle and asserts that:
//   - The new CSV reaches Succeeded state
//   - OLM grants the controller-manager SA all required RBAC (including PVC)
//   - The coordinator Role is created without forbidden errors
//
// Run requirements:
//   - OLM installed in the cluster
//   - OLD_BUNDLE_IMG: pullable bundle image of the previous release (pushed to an accessible registry)
//   - NEW_BUNDLE_IMG: new bundle image pushed to the same registry
//   - OPERATOR_SDK_BIN: path to operator-sdk binary (default: bin/operator-sdk)
//   - BUNDLE_SDK_FLAGS: extra flags passed to operator-sdk run bundle / run bundle-upgrade
//     (default: "--skip-tls-verify" for HTTPS registries; use "--use-http" for plain HTTP registries)
//
// Invoked by CI with label filter "olm-upgrade".
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mariusbertram/oc-mirror-operator/test/utils"
)

var _ = Describe("OLM Upgrade", Ordered, Label("olm-upgrade"), func() {
	const olmNs = "oc-mirror-operator"
	const mtName = "olm-upgrade-rbac-test"

	var (
		oldBundleImg string
		newBundleImg string
		sdkBin       string
		sdkFlags     []string
	)

	BeforeAll(func() {
		oldBundleImg = os.Getenv("OLD_BUNDLE_IMG")
		newBundleImg = os.Getenv("NEW_BUNDLE_IMG")
		sdkBin = os.Getenv("OPERATOR_SDK_BIN")
		if sdkBin == "" {
			sdkBin = "bin/operator-sdk"
		}
		if oldBundleImg == "" || newBundleImg == "" {
			Skip("OLD_BUNDLE_IMG and NEW_BUNDLE_IMG must be set for OLM upgrade tests")
		}

		// BUNDLE_SDK_FLAGS controls registry access: "--use-http" for plain HTTP
		// registries (e.g. localhost:5001 in CI), "--skip-tls-verify" for HTTPS
		// registries with self-signed certs. Defaults to "--skip-tls-verify".
		rawFlags := os.Getenv("BUNDLE_SDK_FLAGS")
		if rawFlags == "" {
			rawFlags = "--skip-tls-verify"
		}
		sdkFlags = strings.Fields(rawFlags)

		By("ensuring the operator namespace exists")
		_, _ = utils.Run(exec.Command("kubectl", "create", "namespace", olmNs))
	})

	AfterAll(func() {
		By("dumping OLM operator state for diagnostics")
		dumpOLMDiagnostics(olmNs)

		By("cleaning up OLM operator install")
		_ = exec.Command(sdkBin, "cleanup", "oc-mirror",
			"--namespace", olmNs, "--timeout", "4m").Run()
		_ = exec.Command("kubectl", "delete", "mirrortarget", mtName,
			"-n", olmNs, "--ignore-not-found=true", "--timeout=60s").Run()
	})

	It("should install the previous bundle version via OLM", func() {
		By(fmt.Sprintf("running old bundle %s", oldBundleImg))
		args := append([]string{"run", "bundle", oldBundleImg,
			"--namespace", olmNs,
			"--timeout", "6m",
			"--image-pull-policy", "IfNotPresent"},
			sdkFlags...)
		cmd := exec.Command(sdkBin, args...)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "old bundle install failed:\n%s", out)

		By("verifying old CSV reaches Succeeded")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "csv",
				"-n", olmNs,
				"-o", "jsonpath={.items[0].status.phase}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("Succeeded"))
		}, 6*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying operator deployment is running after install")
		waitForOperatorReady(olmNs)
	})

	It("should upgrade to the new bundle without RBAC anti-escalation errors", func() {
		By(fmt.Sprintf("upgrading to new bundle %s", newBundleImg))
		args := append([]string{"run", "bundle-upgrade", newBundleImg,
			"--namespace", olmNs,
			"--timeout", "6m",
			"--image-pull-policy", "IfNotPresent"},
			sdkFlags...)
		cmd := exec.Command(sdkBin, args...)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "bundle upgrade failed:\n%s", out)

		By("verifying upgraded CSV reaches Succeeded")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "csv",
				"-n", olmNs,
				"-o", "jsonpath={.items[*].status.phase}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("Succeeded"))
		}, 6*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying operator deployment is running after upgrade")
		waitForOperatorReady(olmNs)
	})

	It("should grant PVC permissions to the controller-manager service account", func() {
		saRef := fmt.Sprintf("system:serviceaccount:%s:oc-mirror-controller-manager", olmNs)
		pvcVerbs := []string{"get", "list", "create", "delete"}

		for _, verb := range pvcVerbs {
			By(fmt.Sprintf("checking can-i %s persistentvolumeclaims", verb))
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "auth", "can-i", verb, "persistentvolumeclaims",
					"--as", saRef, "-n", olmNs)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("yes"),
					"SA missing %s on PVCs", verb)
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		}
	})

	It("should create the coordinator Role with PVC permissions without error", func() {
		By("creating a MirrorTarget to trigger coordinator RBAC setup")
		mtYAML := fmt.Sprintf(`
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: %s
  namespace: %s
spec:
  registry: registry.default.svc.cluster.local:5000/mirror
  insecure: true`, mtName, olmNs)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(mtYAML)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "failed to create MirrorTarget:\n%s", out)

		// The controller creates a Role named "<mirrortarget-name>-coordinator"
		// when it reconciles a MirrorTarget.
		coordinatorRoleName := mtName + "-coordinator"
		By(fmt.Sprintf("verifying coordinator Role %s is created with persistentvolumeclaims permission", coordinatorRoleName))
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "role", coordinatorRoleName,
				"-n", olmNs,
				"-o", "jsonpath={.rules[*].resources}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(),
				"coordinator Role %s not found — check operator logs", coordinatorRoleName)
			g.Expect(out).To(ContainSubstring("persistentvolumeclaims"))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})
})

// waitForOperatorReady waits for the OLM-deployed controller-manager to be
// running. OLM creates a Deployment named "oc-mirror-controller-manager" in the
// operator namespace; we poll for at least one Running pod.
func waitForOperatorReady(ns string) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods",
			"-l", "control-plane=controller-manager",
			"-n", ns,
			"-o", "jsonpath={.items[0].status.phase}")
		out, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "no controller-manager pod found in %s", ns)
		g.Expect(strings.TrimSpace(out)).To(Equal("Running"),
			"controller-manager pod not running in %s, got: %s", ns, out)
	}, 3*time.Minute, 10*time.Second).Should(Succeed())
}

// dumpOLMDiagnostics writes operator state to GinkgoWriter for post-mortem analysis.
func dumpOLMDiagnostics(ns string) {
	for _, args := range [][]string{
		{"get", "pods", "-n", ns, "-o", "wide"},
		{"get", "csv", "-n", ns, "-o", "wide"},
		{"get", "deployments", "-n", ns, "-o", "wide"},
		{"get", "roles,rolebindings", "-n", ns},
		{"get", "mirrortargets", "-n", ns, "-o", "yaml"},
		{"logs", "-l", "control-plane=controller-manager", "-n", ns, "--tail=80", "--all-containers"},
	} {
		cmd := exec.Command("kubectl", args...)
		out, _ := utils.Run(cmd)
		_, _ = fmt.Fprintf(GinkgoWriter, "--- kubectl %s ---\n%s\n", strings.Join(args, " "), out)
	}
}
