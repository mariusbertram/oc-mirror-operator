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

package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestUIConfigurationDefaults validates default values for UIConfiguration fields.
func TestUIConfigurationDefaults(t *testing.T) {
	tests := []struct {
		name     string
		uic      *UIConfiguration
		validate func(t *testing.T, uic *UIConfiguration)
	}{
		{
			name: "Minimal UIConfiguration",
			uic: &UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "minimal-ui",
				},
				Spec: UIConfigurationSpec{},
			},
			validate: func(t *testing.T, uic *UIConfiguration) {
				if uic.Name != "minimal-ui" {
					t.Errorf("Expected name 'minimal-ui', got %q", uic.Name)
				}
				if uic.Status.Phase != "" && uic.Status.Phase != UIConfigurationPhasePending {
					t.Errorf("Expected empty or pending phase, got %q", uic.Status.Phase)
				}
			},
		},
		{
			name: "UIConfiguration with explicit ExposureType",
			uic: &UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "explicit-exposure",
				},
				Spec: UIConfigurationSpec{
					ExposureType: UIExposureTypeIngress,
				},
			},
			validate: func(t *testing.T, uic *UIConfiguration) {
				if uic.Spec.ExposureType != UIExposureTypeIngress {
					t.Errorf("Expected ExposureType 'ingress', got %q", uic.Spec.ExposureType)
				}
			},
		},
		{
			name: "UIConfiguration with all ExposureType values",
			uic: &UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "all-exposure-types",
				},
				Spec: UIConfigurationSpec{},
			},
			validate: func(t *testing.T, uic *UIConfiguration) {
				// Verify all exposure type constants are defined
				exposureTypes := []UIExposureType{
					UIExposureTypeService,
					UIExposureTypeIngress,
					UIExposureTypeRoute,
					UIExposureTypeConsolePlugin,
				}
				for _, et := range exposureTypes {
					if et == "" {
						t.Error("Expected ExposureType constant to be non-empty")
					}
				}
			},
		},
		{
			name: "UIConfiguration with Replicas field",
			uic: &UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "replicas-ui",
				},
				Spec: UIConfigurationSpec{
					Replicas: ptr(int32(3)),
				},
			},
			validate: func(t *testing.T, uic *UIConfiguration) {
				if uic.Spec.Replicas == nil {
					t.Error("Expected Replicas to be set, got nil")
				} else if *uic.Spec.Replicas != 3 {
					t.Errorf("Expected Replicas 3, got %d", *uic.Spec.Replicas)
				}
			},
		},
		{
			name: "UIConfiguration with TLS disabled",
			uic: &UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "tls-disabled",
				},
				Spec: UIConfigurationSpec{
					TLS: &UITLSConfig{
						Enabled: false,
					},
				},
			},
			validate: func(t *testing.T, uic *UIConfiguration) {
				if uic.Spec.TLS == nil {
					t.Error("Expected TLS to be set")
				} else if uic.Spec.TLS.Enabled {
					t.Error("Expected TLS.Enabled to be false")
				}
			},
		},
		{
			name: "UIConfiguration with TLS enabled",
			uic: &UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "tls-enabled",
				},
				Spec: UIConfigurationSpec{
					TLS: &UITLSConfig{
						Enabled: true,
						CertSecretRef: &corev1.LocalObjectReference{
							Name: "tls-secret",
						},
					},
				},
			},
			validate: func(t *testing.T, uic *UIConfiguration) {
				if uic.Spec.TLS == nil {
					t.Error("Expected TLS to be set")
				} else if !uic.Spec.TLS.Enabled {
					t.Error("Expected TLS.Enabled to be true")
				} else if uic.Spec.TLS.CertSecretRef == nil {
					t.Error("Expected CertSecretRef to be set")
				} else if uic.Spec.TLS.CertSecretRef.Name != "tls-secret" {
					t.Errorf("Expected CertSecretRef.Name to be 'tls-secret', got %q", uic.Spec.TLS.CertSecretRef.Name)
				}
			},
		},
		{
			name: "UIConfiguration with Hostname",
			uic: &UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "with-hostname",
				},
				Spec: UIConfigurationSpec{
					Hostname: "dashboard.example.com",
				},
			},
			validate: func(t *testing.T, uic *UIConfiguration) {
				if uic.Spec.Hostname != "dashboard.example.com" {
					t.Errorf("Expected Hostname 'dashboard.example.com', got %q", uic.Spec.Hostname)
				}
			},
		},
		{
			name: "UIConfiguration with IngressClassName",
			uic: &UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "with-ingress-class",
				},
				Spec: UIConfigurationSpec{
					IngressClassName: "nginx",
				},
			},
			validate: func(t *testing.T, uic *UIConfiguration) {
				if uic.Spec.IngressClassName != "nginx" {
					t.Errorf("Expected IngressClassName 'nginx', got %q", uic.Spec.IngressClassName)
				}
			},
		},
		{
			name: "UIConfiguration with RouteName",
			uic: &UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "with-route",
				},
				Spec: UIConfigurationSpec{
					RouteName: "custom-route",
				},
			},
			validate: func(t *testing.T, uic *UIConfiguration) {
				if uic.Spec.RouteName != "custom-route" {
					t.Errorf("Expected RouteName 'custom-route', got %q", uic.Spec.RouteName)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.validate(t, tt.uic)
		})
	}
}

// TestUIConfigurationExposureTypes tests all valid ExposureType values.
func TestUIConfigurationExposureTypes(t *testing.T) {
	tests := []struct {
		name         string
		exposureType UIExposureType
		isValid      bool
	}{
		{
			name:         "Service exposure type",
			exposureType: UIExposureTypeService,
			isValid:      true,
		},
		{
			name:         "Ingress exposure type",
			exposureType: UIExposureTypeIngress,
			isValid:      true,
		},
		{
			name:         "Route exposure type",
			exposureType: UIExposureTypeRoute,
			isValid:      true,
		},
		{
			name:         "ConsolePlugin exposure type",
			exposureType: UIExposureTypeConsolePlugin,
			isValid:      true,
		},
		{
			name:         "Empty exposure type",
			exposureType: "",
			isValid:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uic := &UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-exposure",
				},
				Spec: UIConfigurationSpec{
					ExposureType: tt.exposureType,
				},
			}

			// Check if the exposure type is set
			if (tt.exposureType != "") != tt.isValid {
				t.Fatalf("Unexpected isValid state: exposure type is %q, expected isValid=%v", tt.exposureType, tt.isValid)
			}

			if tt.isValid {
				if uic.Spec.ExposureType != tt.exposureType {
					t.Errorf("Expected ExposureType %q, got %q", tt.exposureType, uic.Spec.ExposureType)
				}
			}
		})
	}
}

// TestUIConfigurationPhases tests all valid UIConfigurationPhase values.
func TestUIConfigurationPhases(t *testing.T) {
	phases := []UIConfigurationPhase{
		UIConfigurationPhasePending,
		UIConfigurationPhaseActive,
		UIConfigurationPhaseFailed,
	}

	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			if phase == "" {
				t.Error("Expected phase to be non-empty")
			}

			// Test that phase can be assigned to status
			uic := &UIConfiguration{
				Status: UIConfigurationStatus{
					Phase: phase,
				},
			}

			if uic.Status.Phase != phase {
				t.Errorf("Expected Phase %q, got %q", phase, uic.Status.Phase)
			}
		})
	}
}

// TestUIConfigurationStatusInitialization tests that status fields initialize properly.
func TestUIConfigurationStatusInitialization(t *testing.T) {
	tests := []struct {
		name     string
		status   UIConfigurationStatus
		validate func(t *testing.T, status UIConfigurationStatus)
	}{
		{
			name:   "Empty status",
			status: UIConfigurationStatus{},
			validate: func(t *testing.T, status UIConfigurationStatus) {
				if status.ObservedGeneration != 0 {
					t.Errorf("Expected ObservedGeneration 0, got %d", status.ObservedGeneration)
				}
				if len(status.Conditions) != 0 {
					t.Errorf("Expected 0 conditions, got %d", len(status.Conditions))
				}
				if status.ExposedURL != "" {
					t.Errorf("Expected empty ExposedURL, got %q", status.ExposedURL)
				}
				if status.Phase != "" {
					t.Errorf("Expected empty Phase, got %q", status.Phase)
				}
				if status.AvailableReplicas != 0 {
					t.Errorf("Expected AvailableReplicas 0, got %d", status.AvailableReplicas)
				}
				if status.DesiredReplicas != 0 {
					t.Errorf("Expected DesiredReplicas 0, got %d", status.DesiredReplicas)
				}
			},
		},
		{
			name: "Status with conditions",
			status: UIConfigurationStatus{
				Conditions: []metav1.Condition{
					{
						Type:   "Ready",
						Status: metav1.ConditionTrue,
					},
				},
			},
			validate: func(t *testing.T, status UIConfigurationStatus) {
				if len(status.Conditions) != 1 {
					t.Errorf("Expected 1 condition, got %d", len(status.Conditions))
				}
				if status.Conditions[0].Type != "Ready" {
					t.Errorf("Expected condition type 'Ready', got %q", status.Conditions[0].Type)
				}
				if status.Conditions[0].Status != metav1.ConditionTrue {
					t.Errorf("Expected condition status True, got %v", status.Conditions[0].Status)
				}
			},
		},
		{
			name: "Status with ExposedURL",
			status: UIConfigurationStatus{
				ExposedURL: "https://dashboard.example.com",
			},
			validate: func(t *testing.T, status UIConfigurationStatus) {
				if status.ExposedURL != "https://dashboard.example.com" {
					t.Errorf("Expected ExposedURL 'https://dashboard.example.com', got %q", status.ExposedURL)
				}
			},
		},
		{
			name: "Status with replicas",
			status: UIConfigurationStatus{
				AvailableReplicas: 2,
				DesiredReplicas:   3,
			},
			validate: func(t *testing.T, status UIConfigurationStatus) {
				if status.AvailableReplicas != 2 {
					t.Errorf("Expected AvailableReplicas 2, got %d", status.AvailableReplicas)
				}
				if status.DesiredReplicas != 3 {
					t.Errorf("Expected DesiredReplicas 3, got %d", status.DesiredReplicas)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.validate(t, tt.status)
		})
	}
}

// TestUIConfigurationStructInitialization tests basic struct initialization.
func TestUIConfigurationStructInitialization(t *testing.T) {
	tests := []struct {
		name     string
		setup    func() *UIConfiguration
		validate func(t *testing.T, uic *UIConfiguration)
	}{
		{
			name: "Minimal UIConfiguration creation",
			setup: func() *UIConfiguration {
				return &UIConfiguration{
					ObjectMeta: metav1.ObjectMeta{
						Name: "minimal",
					},
				}
			},
			validate: func(t *testing.T, uic *UIConfiguration) {
				if uic.Name != "minimal" {
					t.Errorf("Expected name 'minimal', got %q", uic.Name)
				}
				if uic.Spec.ExposureType != "" {
					t.Errorf("Expected empty ExposureType, got %q", uic.Spec.ExposureType)
				}
			},
		},
		{
			name: "Full UIConfiguration creation",
			setup: func() *UIConfiguration {
				return &UIConfiguration{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "full-config",
						Namespace: "default",
					},
					Spec: UIConfigurationSpec{
						ExposureType:     UIExposureTypeIngress,
						Hostname:         "ui.example.com",
						IngressClassName: "nginx",
						Replicas:         ptr(int32(2)),
						TLS: &UITLSConfig{
							Enabled: true,
							CertSecretRef: &corev1.LocalObjectReference{
								Name: "ui-cert",
							},
						},
						Resources: &corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    *mustParse("100m"),
								corev1.ResourceMemory: *mustParse("256Mi"),
							},
						},
					},
					Status: UIConfigurationStatus{
						Phase:              UIConfigurationPhaseActive,
						ExposedURL:         "https://ui.example.com",
						AvailableReplicas:  2,
						DesiredReplicas:    2,
						ObservedGeneration: 1,
					},
				}
			},
			validate: func(t *testing.T, uic *UIConfiguration) {
				if uic.Name != "full-config" {
					t.Errorf("Expected name 'full-config', got %q", uic.Name)
				}
				if uic.Namespace != "default" {
					t.Errorf("Expected namespace 'default', got %q", uic.Namespace)
				}
				if uic.Spec.ExposureType != UIExposureTypeIngress {
					t.Errorf("Expected ExposureType 'ingress', got %q", uic.Spec.ExposureType)
				}
				if uic.Spec.Hostname != "ui.example.com" {
					t.Errorf("Expected Hostname 'ui.example.com', got %q", uic.Spec.Hostname)
				}
				if uic.Spec.Replicas == nil || *uic.Spec.Replicas != 2 {
					t.Errorf("Expected Replicas 2, got %v", uic.Spec.Replicas)
				}
				if uic.Spec.TLS == nil || !uic.Spec.TLS.Enabled {
					t.Error("Expected TLS to be enabled")
				}
				if uic.Status.Phase != UIConfigurationPhaseActive {
					t.Errorf("Expected Phase 'active', got %q", uic.Status.Phase)
				}
				if uic.Status.ExposedURL != "https://ui.example.com" {
					t.Errorf("Expected ExposedURL 'https://ui.example.com', got %q", uic.Status.ExposedURL)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uic := tt.setup()
			tt.validate(t, uic)
		})
	}
}

// TestUIIssuerRef tests cert-manager IssuerRef configuration.
func TestUIIssuerRef(t *testing.T) {
	tests := []struct {
		name     string
		issuer   *UIIssuerRef
		validate func(t *testing.T, issuer *UIIssuerRef)
	}{
		{
			name: "IssuerRef with Issuer kind",
			issuer: &UIIssuerRef{
				Name: "letsencrypt-prod",
				Kind: "Issuer",
			},
			validate: func(t *testing.T, issuer *UIIssuerRef) {
				if issuer.Name != "letsencrypt-prod" {
					t.Errorf("Expected Name 'letsencrypt-prod', got %q", issuer.Name)
				}
				if issuer.Kind != "Issuer" {
					t.Errorf("Expected Kind 'Issuer', got %q", issuer.Kind)
				}
			},
		},
		{
			name: "IssuerRef with ClusterIssuer kind",
			issuer: &UIIssuerRef{
				Name: "letsencrypt-cluster",
				Kind: "ClusterIssuer",
			},
			validate: func(t *testing.T, issuer *UIIssuerRef) {
				if issuer.Name != "letsencrypt-cluster" {
					t.Errorf("Expected Name 'letsencrypt-cluster', got %q", issuer.Name)
				}
				if issuer.Kind != "ClusterIssuer" {
					t.Errorf("Expected Kind 'ClusterIssuer', got %q", issuer.Kind)
				}
			},
		},
		{
			name: "IssuerRef with only name (kind defaults to Issuer)",
			issuer: &UIIssuerRef{
				Name: "default-issuer",
			},
			validate: func(t *testing.T, issuer *UIIssuerRef) {
				if issuer.Name != "default-issuer" {
					t.Errorf("Expected Name 'default-issuer', got %q", issuer.Name)
				}
				if issuer.Kind != "" && issuer.Kind != "Issuer" {
					t.Errorf("Expected Kind to be empty or 'Issuer', got %q", issuer.Kind)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.validate(t, tt.issuer)
		})
	}
}

// TestUIConfigurationList tests the UIConfigurationList structure.
func TestUIConfigurationList(t *testing.T) {
	uicList := &UIConfigurationList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "mirror.openshift.io/v1alpha1",
			Kind:       "UIConfigurationList",
		},
		ListMeta: metav1.ListMeta{
			ResourceVersion: "12345",
		},
		Items: []UIConfiguration{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "ui-1",
				},
				Spec: UIConfigurationSpec{
					ExposureType: UIExposureTypeService,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "ui-2",
				},
				Spec: UIConfigurationSpec{
					ExposureType: UIExposureTypeIngress,
				},
			},
		},
	}

	if len(uicList.Items) != 2 {
		t.Errorf("Expected 2 items, got %d", len(uicList.Items))
	}

	if uicList.Items[0].Name != "ui-1" {
		t.Errorf("Expected first item name 'ui-1', got %q", uicList.Items[0].Name)
	}

	if uicList.Items[1].Spec.ExposureType != UIExposureTypeIngress {
		t.Errorf("Expected second item ExposureType 'ingress', got %q", uicList.Items[1].Spec.ExposureType)
	}
}

// TestUIConfigurationResourceRequirements tests ResourceRequirements handling.
func TestUIConfigurationResourceRequirements(t *testing.T) {
	tests := []struct {
		name      string
		resources *corev1.ResourceRequirements
		validate  func(t *testing.T, resources *corev1.ResourceRequirements)
	}{
		{
			name:      "Nil resources",
			resources: nil,
			validate: func(t *testing.T, resources *corev1.ResourceRequirements) {
				if resources != nil {
					t.Error("Expected nil resources")
				}
			},
		},
		{
			name: "Resources with requests only",
			resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    *mustParse("100m"),
					corev1.ResourceMemory: *mustParse("256Mi"),
				},
			},
			validate: func(t *testing.T, resources *corev1.ResourceRequirements) {
				if resources == nil {
					t.Error("Expected resources to be set")
				} else if resources.Requests == nil {
					t.Error("Expected requests to be set")
				} else if len(resources.Requests) != 2 {
					t.Errorf("Expected 2 resources, got %d", len(resources.Requests))
				}
			},
		},
		{
			name: "Resources with requests and limits",
			resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    *mustParse("100m"),
					corev1.ResourceMemory: *mustParse("256Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    *mustParse("500m"),
					corev1.ResourceMemory: *mustParse("512Mi"),
				},
			},
			validate: func(t *testing.T, resources *corev1.ResourceRequirements) {
				if resources == nil {
					t.Error("Expected resources to be set")
				} else if resources.Limits == nil {
					t.Error("Expected limits to be set")
				} else if len(resources.Limits) != 2 {
					t.Errorf("Expected 2 limit resources, got %d", len(resources.Limits))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uic := &UIConfiguration{
				Spec: UIConfigurationSpec{
					Resources: tt.resources,
				},
			}
			tt.validate(t, uic.Spec.Resources)
		})
	}
}

// Helper function to create pointer to int32
func ptr(i int32) *int32 {
	return &i
}

// Helper function to parse quantity string
func mustParse(s string) *resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic(err)
	}
	return &q
}
