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
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mariusbertram/oc-mirror-operator/test/utils"
)

// operatorNamespace is the namespace where the controller-manager runs.
const operatorNamespace = "oc-mirror-system"

// envTrue is the string value used to enable boolean environment variable flags.
const envTrue = "true"

var (
	// Optional Environment Variables:
	// - CERT_MANAGER_INSTALL_SKIP=true: Skips CertManager installation during test setup.
	// These variables are useful if CertManager is already installed, avoiding
	// re-installation and conflicts.
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == envTrue
	// isCertManagerAlreadyInstalled will be set true when CertManager CRDs be found on the cluster
	isCertManagerAlreadyInstalled = false

	// skipClusterSetup=true skips docker-build + Kind image loading.
	// Set this when the image was already built and loaded in a prior CI step.
	skipClusterSetup = os.Getenv("SKIP_CLUSTER_SETUP") == envTrue

	// skipOperatorDeploy=true skips CRD installation and operator deployment.
	// Set this when running only non-cluster tests (catalog FBC, Cincinnati API)
	// on a machine without a Kubernetes cluster: SKIP_OPERATOR_DEPLOY=true.
	skipOperatorDeploy = os.Getenv("SKIP_OPERATOR_DEPLOY") == envTrue

	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectImage = "example.com/oc-mirror:v0.0.1"
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purposed to be used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting oc-mirror integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	if !skipClusterSetup {
		By("building the manager(Operator) image")
		cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
		_, err := utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

		By("loading the manager(Operator) image on Kind")
		// Pass the podman provider env var through if the Makefile set it.
		if p := os.Getenv("KIND_EXPERIMENTAL_PROVIDER"); p != "" {
			Expect(os.Setenv("KIND_EXPERIMENTAL_PROVIDER", p)).To(Succeed())
		}
		err = utils.LoadImageToKindClusterWithName(projectImage)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")
	}

	// Setup CertManager before the suite if not skipped and if not already installed.
	if !skipCertManagerInstall {
		By("checking if cert manager is installed already")
		isCertManagerAlreadyInstalled = utils.IsCertManagerCRDsInstalled()
		if !isCertManagerAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing CertManager...\n")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CertManager is already installed. Skipping installation...\n")
		}
	}

	// Install CRDs and deploy the operator once for the entire suite.
	// All cluster test suites share this single operator instance — there is no
	// per-Describe install/deploy, which prevents CRDs from cycling through
	// "terminating" state between suites.
	if !skipOperatorDeploy {
		By("installing CRDs")
		cmd := exec.Command("make", "install")
		_, err := utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the operator")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy the operator")

		By("waiting for controller-manager to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-n", operatorNamespace,
				"-o", "jsonpath={.items[0].status.phase}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Running"), "controller-manager pod not running")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	}
})

var _ = AfterSuite(func() {
	// Teardown CertManager after the suite if not skipped and if it was not already installed
	if !skipCertManagerInstall && !isCertManagerAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling CertManager...\n")
		utils.UninstallCertManager()
	}

	if !skipOperatorDeploy {
		By("undeploying the operator")
		_ = exec.Command("make", "undeploy").Run()
	}
})
