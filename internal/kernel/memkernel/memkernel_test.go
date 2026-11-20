package memkernel_test

import (
	"testing"

	"nomanqureshi.dev/hpyo-agent/internal/kernel/conformance"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/memkernel"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(*testing.T) conformance.Harness { return memkernel.New() })
}
