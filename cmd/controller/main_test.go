package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// findProjectRoot finds the project root by looking for go.mod
func findProjectRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	// Start from current directory and walk up until we find go.mod
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached root, go.mod not found
			break
		}
		dir = parent
	}

	// Fallback: assume we're in or near the project root
	return wd, nil
}

// TestControllerBinaryCompiles verifies that the controller binary can be built and run
func TestControllerBinaryCompiles(t *testing.T) {
	projectRoot, err := findProjectRoot()
	if err != nil {
		t.Fatalf("Failed to find project root: %v", err)
	}

	// Build the controller binary
	outPath := filepath.Join(projectRoot, "bin", "controller")
	cmd := exec.Command("go", "build", "-o", outPath, "./cmd/controller/")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.Dir = projectRoot
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to build controller binary: %v", err)
	}

	// Verify binary exists
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("Controller binary not found: %v", err)
	}

	t.Log("✓ Controller binary compiled successfully")
}

// TestControllerHelp verifies that the controller binary accepts --help flag
// and only shows controller-specific flags (no worker/manager/cleanup flags)
func TestControllerHelp(t *testing.T) {
	projectRoot, err := findProjectRoot()
	if err != nil {
		t.Fatalf("Failed to find project root: %v", err)
	}

	binPath := filepath.Join(projectRoot, "bin", "controller")
	cmd := exec.Command(binPath, "-help")
	cmd.Dir = projectRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to run controller -help: %v\n%s", err, output)
	}

	helpText := string(output)

	// Verify controller flags are present
	expectedFlags := []string{
		"metrics-bind-address",
		"health-probe-bind-address",
		"leader-elect",
	}

	for _, flag := range expectedFlags {
		if !contains(helpText, flag) {
			t.Errorf("Expected flag '%s' not found in help output", flag)
		}
	}

	// Verify NO worker/manager/cleanup subcommand flags
	forbiddenPatterns := []string{
		" worker",  // standalone word "worker"
		" cleanup", // standalone word "cleanup"
		"--resource-api",
		"--batch-size",
		"--target-name",
	}

	for _, pattern := range forbiddenPatterns {
		if contains(helpText, pattern) {
			t.Errorf("Unexpected flag pattern '%s' found in help output (should be removed)", pattern)
		}
	}

	t.Log("✓ Controller help shows only controller flags")
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
