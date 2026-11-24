// Package storetest provides the shared contract test suite for Store impls.
//
// Both the in-memory and SQLite impls (and any future impl) are expected to
// pass RunContract. New cases land here, not in any single impl's _test.go.
package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/medios-lab/medios-sdk-go/store"
)

// RunContract executes the v0.1 Store contract suite against the given factory.
// factory must return a fresh Store on each call; sub-tests do not share state.
//
// Counted toward v0.1 INTENT-ONLY coverage:
//   - 4 Singleton cases (SingletonRepositoryTest.scala)
//   - 8 Blob bucket cases (PersistenceStoreTest.scala basic CRUD subset)
//   - 1 Lifecycle case (Close is callable)
func RunContract(t *testing.T, factory func(t *testing.T) store.Store) {
	t.Helper()

	// ── Singleton (SingletonRepositoryTest.scala) ──────────────────────────

	// "return none if nothing has been stored"
	t.Run("Singleton/GetEmpty", func(t *testing.T) {
		s := factory(t)
		_, err := s.GetSingleton(context.Background(), "framework_id")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetSingleton: want ErrNotFound, got %v", err)
		}
	})

	// "delete should succeed if nothing has been stored"
	t.Run("Singleton/DeleteEmpty", func(t *testing.T) {
		s := factory(t)
		if err := s.DeleteSingleton(context.Background(), "framework_id"); err != nil {
			t.Fatalf("DeleteSingleton on empty: want nil, got %v", err)
		}
	})

	// "retrieve the previously stored value"
	t.Run("Singleton/PutThenGet", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		want := "9e8d0946-6f25-411a-abeb-e168bbd225b0-0001"
		if err := s.PutSingleton(ctx, "framework_id", want); err != nil {
			t.Fatalf("PutSingleton: %v", err)
		}
		got, err := s.GetSingleton(ctx, "framework_id")
		if err != nil {
			t.Fatalf("GetSingleton: %v", err)
		}
		if got != want {
			t.Fatalf("GetSingleton: want %q, got %q", want, got)
		}
	})

	// "delete a previously stored value should unset the value"
	t.Run("Singleton/PutDeleteGet", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		_ = s.PutSingleton(ctx, "framework_id", "abc")
		if err := s.DeleteSingleton(ctx, "framework_id"); err != nil {
			t.Fatalf("DeleteSingleton: %v", err)
		}
		_, err := s.GetSingleton(ctx, "framework_id")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetSingleton after delete: want ErrNotFound, got %v", err)
		}
	})

	// ── Blob bucket (PersistenceStoreTest.scala basic CRUD) ────────────────

	// "not fail if the key doesn't exist"
	t.Run("Blob/GetEmpty", func(t *testing.T) {
		s := factory(t)
		_, err := s.GetBlob(context.Background(), "apps", "missing")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetBlob: want ErrNotFound, got %v", err)
		}
	})

	// "have no ids"
	t.Run("Blob/ListEmpty", func(t *testing.T) {
		s := factory(t)
		got, err := s.ListBlob(context.Background(), "apps")
		if err != nil {
			t.Fatalf("ListBlob: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListBlob on empty: want empty, got %v", got)
		}
	})

	// "create and list an object"
	t.Run("Blob/PutThenGet", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		if err := s.PutBlob(ctx, "apps", "/foo", `{"id":"/foo"}`); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
		got, err := s.GetBlob(ctx, "apps", "/foo")
		if err != nil {
			t.Fatalf("GetBlob: %v", err)
		}
		if got != `{"id":"/foo"}` {
			t.Fatalf("GetBlob: want %q, got %q", `{"id":"/foo"}`, got)
		}
	})

	// "update an object" (without versioning, v0.1 doesn't carry version history)
	t.Run("Blob/PutOverwrites", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		_ = s.PutBlob(ctx, "apps", "/foo", "v1")
		_ = s.PutBlob(ctx, "apps", "/foo", "v2")
		got, _ := s.GetBlob(ctx, "apps", "/foo")
		if got != "v2" {
			t.Fatalf("PutBlob overwrite: want v2, got %q", got)
		}
	})

	// "delete idempotently"
	t.Run("Blob/DeleteIdempotent", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		if err := s.DeleteBlob(ctx, "apps", "missing"); err != nil {
			t.Fatalf("DeleteBlob on empty: %v", err)
		}
		_ = s.PutBlob(ctx, "apps", "/foo", "v1")
		if err := s.DeleteBlob(ctx, "apps", "/foo"); err != nil {
			t.Fatalf("DeleteBlob first: %v", err)
		}
		if err := s.DeleteBlob(ctx, "apps", "/foo"); err != nil {
			t.Fatalf("DeleteBlob second (idempotent): %v", err)
		}
	})

	// "stored objects should list in the ids and all"
	t.Run("Blob/ListPopulated", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		_ = s.PutBlob(ctx, "apps", "/a", "1")
		_ = s.PutBlob(ctx, "apps", "/b", "2")
		_ = s.PutBlob(ctx, "apps", "/c", "3")

		got, _ := s.ListBlob(ctx, "apps")
		want := map[string]string{"/a": "1", "/b": "2", "/c": "3"}
		if !mapEq(got, want) {
			t.Fatalf("ListBlob populated: want %v, got %v", want, got)
		}

		_ = s.DeleteBlob(ctx, "apps", "/b")
		got, _ = s.ListBlob(ctx, "apps")
		want = map[string]string{"/a": "1", "/c": "3"}
		if !mapEq(got, want) {
			t.Fatalf("ListBlob after delete: want %v, got %v", want, got)
		}
	})

	// Bucket isolation, two buckets don't bleed into each other.
	t.Run("Blob/BucketsAreIsolated", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		_ = s.PutBlob(ctx, "apps", "x", "1")
		_ = s.PutBlob(ctx, "tasks", "x", "2")
		gotApps, _ := s.GetBlob(ctx, "apps", "x")
		gotTasks, _ := s.GetBlob(ctx, "tasks", "x")
		if gotApps != "1" || gotTasks != "2" {
			t.Fatalf("bucket isolation: apps=%q tasks=%q", gotApps, gotTasks)
		}
	})

	// Close is callable; after Close behavior is undefined and not asserted.
	t.Run("Lifecycle/Close", func(t *testing.T) {
		s := factory(t)
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	// Closing twice is a no-op that returns nil (idempotent), matching Go
	// conventions for resource handles. Pinned here so the contract is
	// explicit rather than driver-dependent.
	t.Run("Lifecycle/CloseTwice", func(t *testing.T) {
		s := factory(t)
		if err := s.Close(); err != nil {
			t.Fatalf("Close (first): %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close (second) must be a no-op returning nil, got: %v", err)
		}
	})
}

func mapEq(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
