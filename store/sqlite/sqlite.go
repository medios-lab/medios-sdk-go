// Package sqlite is the production Store implementation for the
// medios-sdk-go.
//
// Backed by modernc.org/sqlite (pure-Go SQLite, no CGO). WAL mode enabled
// for many-reader + one-writer concurrency. Schema lives in migrations/
// applied on startup; the file is embedded so the binary has no external
// dependency on a migrations directory.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/medios-lab/medios-sdk-go/store"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is the SQLite-backed Store implementation. Safe for concurrent use
// from the consumer's perspective; modernc.org/sqlite serializes writes
// internally.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path, enables WAL, and
// applies any pending migrations. Returns a ready-to-use Store.
//
// path may be ":memory:" for an ephemeral DB (useful for tests that want
// to exercise the SQLite code path without disk I/O).
func Open(path string) (*Store, error) {
	// Pragmas in the DSN are applied before the connection is handed back.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	// modernc.org/sqlite's WAL mode requires a single connection for in-memory
	// databases; setting it conservatively for all cases keeps the contract
	// simple and predictable.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var existing string
		err := s.db.QueryRowContext(ctx,
			`SELECT version FROM schema_migrations WHERE version = ?`,
			name,
		).Scan(&existing)
		if err == nil {
			continue // already applied
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("query schema_migrations: %w", err)
		}

		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := s.db.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES (?)`,
			name,
		); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) PutSingleton(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO singletons (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

func (s *Store) GetSingleton(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM singletons WHERE key = ?`, key,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return value, err
}

func (s *Store) DeleteSingleton(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM singletons WHERE key = ?`, key)
	return err
}

func (s *Store) PutBlob(ctx context.Context, bucket, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO blobs (bucket, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(bucket, key) DO UPDATE SET value = excluded.value`,
		bucket, key, value,
	)
	return err
}

func (s *Store) GetBlob(ctx context.Context, bucket, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM blobs WHERE bucket = ? AND key = ?`,
		bucket, key,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return value, err
}

func (s *Store) DeleteBlob(ctx context.Context, bucket, key string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM blobs WHERE bucket = ? AND key = ?`,
		bucket, key,
	)
	return err
}

func (s *Store) ListBlob(ctx context.Context, bucket string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM blobs WHERE bucket = ?`, bucket,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }
