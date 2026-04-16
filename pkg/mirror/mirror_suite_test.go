package mirror

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
)

func TestMirror(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Mirror Suite")
}
