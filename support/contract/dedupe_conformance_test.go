package contract_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/support/contract"
	"github.com/asteby/metacore-kernel/support/contract/contracttest"
)

func TestMemoryDeduperConformance(t *testing.T) {
	contracttest.RunDedupe(t, func(_ *testing.T, clock *contracttest.Clock, max int) contract.DeliveryDeduper {
		return &contract.MemoryDeduper{MaxEntries: max, Now: clock.Now}
	})
}
