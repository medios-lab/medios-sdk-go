package memory_test

import (
	"testing"

	"github.com/medios-lab/medios-sdk-go/store"
	"github.com/medios-lab/medios-sdk-go/store/memory"
	"github.com/medios-lab/medios-sdk-go/store/storetest"
)

func TestMemoryStore(t *testing.T) {
	storetest.RunContract(t, func(_ *testing.T) store.Store { return memory.New() })
}
