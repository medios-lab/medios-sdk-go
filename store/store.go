// Package store defines the medios-sdk-go persistence contract.
//
// v0.1 surface (the minimum required for the scheduler + REST API to
// recover state across restarts):
//
//   - Singleton: small named string values (framework_id, schema_version...)
//   - Blob bucket: namespaced collections of (key, value) string blobs that
//     each framework's typed wrappers layer over.
//
// The interface is intentionally narrow. It does NOT expose generic
// KV semantics; SQL is an implementation detail. The shared interface stays
// generic: framework-specific typed accessors live in each framework's
// wrapper over this Store, never on the SDK interface (that would bake one
// framework's object model into the SDK every other framework imports).
package store

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get when no record exists for the requested key.
// Callers should not match this with == ; use errors.Is.
var ErrNotFound = errors.New("store: not found")

// Store is the persistence contract. Implementations:
//
//   - store/memory, in-process, no persistence; for tests
//   - store/sqlite, embedded SQLite with WAL; the v1 production impl
type Store interface {
	// PutSingleton inserts or replaces the value for the given key.
	PutSingleton(ctx context.Context, key, value string) error

	// GetSingleton returns the value for the given key.
	// Returns ErrNotFound if the key has no stored value.
	GetSingleton(ctx context.Context, key string) (string, error)

	// DeleteSingleton removes the value for the given key.
	// A second delete (or delete of a never-stored key) returns nil, idempotent.
	DeleteSingleton(ctx context.Context, key string) error

	// PutBlob inserts or replaces a value in the given bucket under the given key.
	PutBlob(ctx context.Context, bucket, key, value string) error

	// GetBlob returns the value for (bucket, key).
	// Returns ErrNotFound if no such entry exists.
	GetBlob(ctx context.Context, bucket, key string) (string, error)

	// DeleteBlob removes the value for (bucket, key). Idempotent.
	DeleteBlob(ctx context.Context, bucket, key string) error

	// ListBlob returns all (key, value) entries in the given bucket.
	// Returns an empty map (not nil) for an unknown / empty bucket.
	ListBlob(ctx context.Context, bucket string) (map[string]string, error)

	// Close releases underlying resources (file handles, connections).
	// After Close, no other method is valid.
	Close() error
}
