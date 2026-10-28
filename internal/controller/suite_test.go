package controller

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestControllers runs the Ginkgo specs for both reconcilers. Each spec builds its
// own harness (fake Kubernetes client + cloudflare.Fake), so specs are independent.
func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}
