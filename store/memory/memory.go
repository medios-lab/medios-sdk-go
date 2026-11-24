// Package memory provides an in-process Store implementation for testing.
//
// Not durable: all state is lost on process exit. Used by:
//   - the Store contract test suite (validates the implementation against the
//     same assertions the SQLite impl runs through)
//   - higher-layer unit tests that need a working Store without disk I/O
package memory

import (
	"context"
	"sync"

	"github.com/medios-lab/medios-sdk-go/store"
)

// Store is the in-memory implementation of store.Store. Safe for concurrent use.
type Store struct {
	mu         sync.RWMutex
	singletons map[string]string
	blobs      map[string]map[string]string // bucket → key → value
	closed     bool
}

// New returns an empty in-memory Store.
func New() *Store {
	return &Store{
		singletons: map[string]string{},
		blobs:      map[string]map[string]string{},
	}
}

func (s *Store) PutSingleton(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.singletons[key] = value
	return nil
}

func (s *Store) GetSingleton(_ context.Context, key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.singletons[key]
	if !ok {
		return "", store.ErrNotFound
	}
	return v, nil
}

func (s *Store) DeleteSingleton(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.singletons, key)
	return nil
}

func (s *Store) PutBlob(_ context.Context, bucket, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[bucket]
	if !ok {
		b = map[string]string{}
		s.blobs[bucket] = b
	}
	b[key] = value
	return nil
}

func (s *Store) GetBlob(_ context.Context, bucket, key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.blobs[bucket]
	if !ok {
		return "", store.ErrNotFound
	}
	v, ok := b[key]
	if !ok {
		return "", store.ErrNotFound
	}
	return v, nil
}

func (s *Store) DeleteBlob(_ context.Context, bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.blobs[bucket]; ok {
		delete(b, key)
	}
	return nil
}

func (s *Store) ListBlob(_ context.Context, bucket string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]string{}
	for k, v := range s.blobs[bucket] {
		out[k] = v
	}
	return out, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
