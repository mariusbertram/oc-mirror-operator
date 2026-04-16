package e2e_flow

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
)

func TestE2EFlow(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Flow Suite")
}
