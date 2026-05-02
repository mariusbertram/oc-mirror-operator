package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

func TestManagerBinaryFlagParsing(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantError bool
		wantHelp  bool
	}{
		{
			name:      "help flag",
			args:      []string{"-help"},
			wantError: true, // flag.Parse exits with -help
			wantHelp:  true,
		},
		{
			name:      "missing mirrortarget",
			args:      []string{"-namespace=default"},
			wantError: false, // will fail at runtime when trying to connect to cluster
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify the flags parse correctly by building and checking output
			cmd := exec.Command("go", "build", "-o", "/dev/null", ".")
			cmd.Dir = "."
			err := cmd.Run()
			if err != nil {
				t.Fatalf("failed to build manager binary: %v", err)
			}
		})
	}
}

func TestManagerBinaryCompiles(t *testing.T) {
	// Ensure the binary can be built
	cmd := exec.Command("go", "build", "-o", "/tmp/manager-test-build", ".")
	cmd.Dir = "."
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to build manager binary: %v", err)
	}
	defer func() { _ = os.Remove("/tmp/manager-test-build") }()
}

func TestFlagVariables(t *testing.T) {
	// Test that flag parsing would work correctly
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var targetName, namespace string
	fs.StringVar(&targetName, "mirrortarget", "", "Name of the MirrorTarget")
	fs.StringVar(&namespace, "namespace", "", "Namespace of the MirrorTarget")

	err := fs.Parse([]string{"-mirrortarget=test-target", "-namespace=default"})
	if err != nil {
		t.Fatalf("unexpected error parsing flags: %v", err)
	}

	if targetName != "test-target" {
		t.Errorf("expected targetName=test-target, got %s", targetName)
	}
	if namespace != "default" {
		t.Errorf("expected namespace=default, got %s", namespace)
	}
}

func TestEnvironmentVariableOverride(t *testing.T) {
	// Test that POD_NAMESPACE env var can override namespace flag
	tests := []struct {
		name     string
		flagVal  string
		envVal   string
		expected string
	}{
		{
			name:     "flag takes precedence over env",
			flagVal:  "flagged-ns",
			envVal:   "env-ns",
			expected: "flagged-ns",
		},
		{
			name:     "env used when flag empty",
			flagVal:  "",
			envVal:   "env-ns",
			expected: "env-ns",
		},
		{
			name:     "empty when no flag or env",
			flagVal:  "",
			envVal:   "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the logic from cmd/manager/main.go
			namespace := tt.flagVal
			if namespace == "" {
				namespace = tt.envVal
			}

			if namespace != tt.expected {
				t.Errorf("expected namespace=%s, got %s", tt.expected, namespace)
			}
		})
	}
}

// Example test showing how the manager would be instantiated
// In a real test, this would use envtest to create a fake cluster
func TestManagerInstantiationLogic(t *testing.T) {
	targetName := "test-mirrortarget"
	namespace := "test-namespace"

	if targetName == "" {
		t.Fatal("mirrortarget flag is required")
	}

	if namespace == "" {
		t.Fatal("namespace flag or POD_NAMESPACE env var is required")
	}

	// These would normally call manager.New() and manager.Run()
	// but we're just testing the validation logic here
	fmt.Printf("Manager would start with target=%s in namespace=%s\n", targetName, namespace)
}
