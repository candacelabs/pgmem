package pgmem_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestPGMem(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "PGMem Suite")
}
