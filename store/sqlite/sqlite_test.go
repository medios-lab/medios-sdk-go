package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/medios-lab/medios-sdk-go/store"
	"github.com/medios-lab/medios-sdk-go/store/sqlite"
	"github.com/medios-lab/medios-sdk-go/store/storetest"
)

// TestSQLiteStore_OnDisk runs the shared contract suite against an on-disk DB.
// Each sub-test gets a fresh DB file in t.TempDir() so they don't share state.
func TestSQLiteStore_OnDisk(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) store.Store {
		path := filepath.Join(t.TempDir(), "sdk.db")
		s, err := sqlite.Open(path)
		if err != nil {
			t.Fatalf("sqlite.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// TestSQLiteStore_InMemory exercises the same contract using ":memory:".
// Useful as a fast sanity check that doesn't touch the filesystem.
func TestSQLiteStore_InMemory(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) store.Store {
		s, err := sqlite.Open(":memory:")
		if err != nil {
			t.Fatalf("sqlite.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
