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

package controller

import (
	"context"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("Dashboard Controller", func() {
	const (
		timeout            = 10 * time.Second
		interval           = 250 * time.Millisecond
		dashboardNamespace = "default"
	)

	Context("Reconcile basic flow", func() {
		ctx := context.Background()

		BeforeEach(func() {
			Expect(os.Setenv("DASHBOARD_IMAGE", "test-dashboard:latest")).To(Succeed())
			Expect(os.Setenv("OAUTH_PROXY_IMAGE", "test-oauth-proxy:latest")).To(Succeed())
		})

		It("should create Dashboard Deployment, Service, and Route", func() {
			reconciler := &DashboardReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Namespace: dashboardNamespace,
			}

			By("reconciling the dashboard")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Dashboard Deployment exists")
			deployment := &appsv1.Deployment{}
			deploymentName := types.NamespacedName{Name: "oc-mirror-dashboard", Namespace: dashboardNamespace}
			Eventually(func() error {
				return k8sClient.Get(ctx, deploymentName, deployment)
			}, timeout, interval).Should(Succeed())

			Expect(deployment.Spec.Replicas).NotTo(BeNil())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))
			Expect(deployment.Spec.Template.Spec.ServiceAccountName).To(Equal("oc-mirror-dashboard"))

			By("verifying the Dashboard Service exists")
			service := &corev1.Service{}
			serviceName := types.NamespacedName{Name: "oc-mirror-dashboard", Namespace: dashboardNamespace}
			Eventually(func() error {
				return k8sClient.Get(ctx, serviceName, service)
			}, timeout, interval).Should(Succeed())
			Expect(service.Spec.Selector).To(HaveKeyWithValue("app", "oc-mirror-dashboard"))

			By("verifying ServiceAccount exists")
			sa := &corev1.ServiceAccount{}
			saName := types.NamespacedName{Name: "oc-mirror-dashboard", Namespace: dashboardNamespace}
			Eventually(func() error {
				return k8sClient.Get(ctx, saName, sa)
			}, timeout, interval).Should(Succeed())

			By("cleaning up resources")
			_ = k8sClient.Delete(ctx, deployment)
			_ = k8sClient.Delete(ctx, service)
			_ = k8sClient.Delete(ctx, sa)
		})
	})

	Context("ensureClusterRBAC", func() {
		ctx := context.Background()

		It("should create ClusterRole with correct rules", func() {
			reconciler := &DashboardReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Namespace: dashboardNamespace,
			}

			By("ensuring cluster RBAC")
			err := reconciler.ensureClusterRBAC(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the ClusterRole exists")
			cr := &rbacv1.ClusterRole{}
			crName := types.NamespacedName{Name: "oc-mirror-dashboard"}
			Eventually(func() error {
				return k8sClient.Get(ctx, crName, cr)
			}, timeout, interval).Should(Succeed())

			By("verifying the ClusterRole has the correct rules")
			Expect(cr.Rules).To(HaveLen(2))
			// First rule: mirrortargets and imagesets
			Expect(cr.Rules[0].APIGroups).To(ContainElement("mirror.openshift.io"))
			Expect(cr.Rules[0].Resources).To(ContainElements("mirrortargets", "imagesets"))
			Expect(cr.Rules[0].Verbs).To(ContainElements("get", "list", "watch", "update", "patch"))
			// Second rule: configmaps
			Expect(cr.Rules[1].APIGroups).To(ContainElement(""))
			Expect(cr.Rules[1].Resources).To(ContainElement("configmaps"))

			By("verifying the ClusterRoleBinding exists")
			crb := &rbacv1.ClusterRoleBinding{}
			crbName := types.NamespacedName{Name: "oc-mirror-dashboard"}
			Eventually(func() error {
				return k8sClient.Get(ctx, crbName, crb)
			}, timeout, interval).Should(Succeed())

			By("verifying the ClusterRoleBinding references the ClusterRole")
			Expect(crb.RoleRef.Name).To(Equal("oc-mirror-dashboard"))
			Expect(crb.RoleRef.Kind).To(Equal("ClusterRole"))

			By("verifying the ClusterRoleBinding has the correct subject")
			Expect(crb.Subjects).To(HaveLen(1))
			Expect(crb.Subjects[0].Kind).To(Equal("ServiceAccount"))
			Expect(crb.Subjects[0].Name).To(Equal("oc-mirror-dashboard"))
			Expect(crb.Subjects[0].Namespace).To(Equal(dashboardNamespace))

			By("cleaning up resources")
			_ = k8sClient.Delete(ctx, cr)
			_ = k8sClient.Delete(ctx, crb)
		})
	})

	Context("ensureOAuthProxySecret", func() {
		ctx := context.Background()

		It("should create the OAuth proxy secret with session_secret", func() {
			reconciler := &DashboardReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Namespace: dashboardNamespace,
			}

			By("ensuring OAuth proxy secret")
			err := reconciler.ensureOAuthProxySecret(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the secret exists")
			secret := &corev1.Secret{}
			secretName := types.NamespacedName{Name: "oc-mirror-dashboard-proxy", Namespace: dashboardNamespace}
			Eventually(func() error {
				return k8sClient.Get(ctx, secretName, secret)
			}, timeout, interval).Should(Succeed())

			By("verifying the secret has session_secret")
			Expect(secret.Data).To(HaveKey("session_secret"))
			Expect(secret.Data["session_secret"]).To(HaveLen(32))

			By("storing the original session_secret")
			originalSecret := make([]byte, len(secret.Data["session_secret"]))
			copy(originalSecret, secret.Data["session_secret"])

			By("reconciling again and verifying session_secret is not changed")
			err = reconciler.ensureOAuthProxySecret(ctx)
			Expect(err).NotTo(HaveOccurred())

			secret2 := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretName, secret2)).To(Succeed())
			Expect(secret2.Data["session_secret"]).To(Equal(originalSecret))

			By("cleaning up resources")
			_ = k8sClient.Delete(ctx, secret)
		})

		It("should return error if secret creation fails", func() {
			// Create a secret with invalid data
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "oc-mirror-dashboard-proxy",
					Namespace: dashboardNamespace,
				},
				Data: map[string][]byte{
					"session_secret": []byte("initial-value"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			reconciler := &DashboardReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Namespace: dashboardNamespace,
			}

			By("ensuring OAuth proxy secret")
			err := reconciler.ensureOAuthProxySecret(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the session_secret is not overwritten")
			updatedSecret := &corev1.Secret{}
			secretName := types.NamespacedName{Name: "oc-mirror-dashboard-proxy", Namespace: dashboardNamespace}
			Expect(k8sClient.Get(ctx, secretName, updatedSecret)).To(Succeed())
			Expect(updatedSecret.Data["session_secret"]).To(Equal([]byte("initial-value")))

			By("cleaning up resources")
			_ = k8sClient.Delete(ctx, secret)
		})
	})

	Context("Dashboard skipping when DASHBOARD_IMAGE not set", func() {
		ctx := context.Background()

		BeforeEach(func() {
			Expect(os.Unsetenv("DASHBOARD_IMAGE")).To(Succeed())
		})

		It("should skip reconciliation when DASHBOARD_IMAGE is not set", func() {
			reconciler := &DashboardReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Namespace: dashboardNamespace,
			}

			By("reconciling the dashboard without DASHBOARD_IMAGE")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())

			By("verifying no deployment was created")
			deployment := &appsv1.Deployment{}
			deploymentName := types.NamespacedName{Name: "oc-mirror-dashboard", Namespace: dashboardNamespace}
			err = k8sClient.Get(ctx, deploymentName, deployment)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("Plugin deployment", func() {
		ctx := context.Background()

		BeforeEach(func() {
			Expect(os.Setenv("DASHBOARD_IMAGE", "test-dashboard:latest")).To(Succeed())
			Expect(os.Setenv("OAUTH_PROXY_IMAGE", "test-oauth-proxy:latest")).To(Succeed())
		})

		It("should create Plugin Deployment and Service", func() {
			reconciler := &DashboardReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Namespace: dashboardNamespace,
			}

			By("reconciling the dashboard")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Plugin Deployment exists")
			deployment := &appsv1.Deployment{}
			deploymentName := types.NamespacedName{Name: "oc-mirror-dashboard-plugin", Namespace: dashboardNamespace}
			Eventually(func() error {
				return k8sClient.Get(ctx, deploymentName, deployment)
			}, timeout, interval).Should(Succeed())

			Expect(deployment.Spec.Template.Spec.ServiceAccountName).To(Equal("oc-mirror-dashboard"))
			Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(deployment.Spec.Template.Spec.Containers[0].Command).To(ContainElement("plugin"))

			By("verifying the Plugin Service exists")
			service := &corev1.Service{}
			serviceName := types.NamespacedName{Name: "oc-mirror-dashboard-plugin", Namespace: dashboardNamespace}
			Eventually(func() error {
				return k8sClient.Get(ctx, serviceName, service)
			}, timeout, interval).Should(Succeed())
			Expect(service.Spec.Selector).To(HaveKeyWithValue("app", "oc-mirror-dashboard-plugin"))

			By("cleaning up resources")
			_ = k8sClient.Delete(ctx, deployment)
			_ = k8sClient.Delete(ctx, service)
		})
	})
})
