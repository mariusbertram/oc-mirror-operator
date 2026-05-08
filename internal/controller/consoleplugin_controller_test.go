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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("ConsolePlugin Controller", func() {
	const (
		testNamespace = "default"
		testDashImage = "test-dashboard:latest"
	)

	var (
		ctx        = context.Background()
		reconciler *ConsolePluginReconciler
	)

	BeforeEach(func() {
		reconciler = &ConsolePluginReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Namespace: testNamespace,
			DashImage: testDashImage,
		}
	})

	Context("ensureServiceAccount", func() {
		It("should create a ServiceAccount in the namespace", func() {
			By("calling ensureServiceAccount")
			Expect(reconciler.ensureServiceAccount(ctx)).To(Succeed())

			By("verifying the ServiceAccount exists")
			sa := &corev1.ServiceAccount{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginSAName,
				Namespace: testNamespace,
			}, sa)).To(Succeed())
			Expect(sa.Name).To(Equal(pluginSAName))
		})
	})

	Context("ensureRBAC", func() {
		It("should create a Role and RoleBinding in the namespace", func() {
			By("calling ensureRBAC")
			Expect(reconciler.ensureRBAC(ctx)).To(Succeed())

			By("verifying the Role exists")
			role := &rbacv1.Role{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginRoleName,
				Namespace: testNamespace,
			}, role)).To(Succeed())
			Expect(role.Rules).NotTo(BeEmpty())

			By("verifying the RoleBinding exists")
			rb := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginRoleName,
				Namespace: testNamespace,
			}, rb)).To(Succeed())
			Expect(rb.RoleRef.Name).To(Equal(pluginRoleName))
			Expect(rb.Subjects).To(HaveLen(1))
			Expect(rb.Subjects[0].Name).To(Equal(pluginSAName))
		})
	})

	Context("ensureDeployment", func() {
		It("should create a Deployment with the correct container command", func() {
			By("calling ensureDeployment")
			Expect(reconciler.ensureDeployment(ctx)).To(Succeed())

			By("verifying the Deployment exists")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginDeploymentName,
				Namespace: testNamespace,
			}, dep)).To(Succeed())

			By("verifying the container command")
			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := dep.Spec.Template.Spec.Containers[0]
			Expect(container.Command).To(Equal([]string{"/dashboard", "plugin"}))
			Expect(container.Image).To(Equal(testDashImage))
		})
	})
})
