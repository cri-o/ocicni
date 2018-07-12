package ocicni

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"testing"
)

func TestOcicni(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ocicni")
}
