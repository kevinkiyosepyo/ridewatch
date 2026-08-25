// Package store is the Postgres persistence layer: schema migrations, weekly
// partition management, streaming static-GTFS loads, and the implementations
// of every domain repository interface.
package store

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinkiyosepyo/ridewatch/db"
	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// Store wraps a pgxpool and implements domain.ScheduleRepo, domain.EventStore,
// domain.RawArchive, domain.StopQueries, and domain.SubscriptionStore.
type Store struct {
	pool *pgxpool.Pool

	// Bounded TripSchedule cache. Entries belong to tripCacheVersion; the
	// whole map is cleared when a different schedule version is requested.
	cacheMu          sync.Mutex
	tripCache        map[string]*domain.TripSchedule
	tripCacheVersion int64
}

var (
	_ domain.ScheduleRepo      = (*Store)(nil)
	_ domain.EventStore        = (*Store)(nil)
	_ domain.RawArchive        = (*Store)(nil)
	_ domain.StopQueries       = (*Store)(nil)
	_ domain.SubscriptionStore = (*Store)(nil)
)

// Open connects a pgxpool to databaseURL. Sessions run with timezone=UTC so
// date literals and timestamps never depend on server configuration.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: parse database url: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool, tripCache: make(map[string]*domain.TripSchedule)}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Migrate applies every not-yet-applied file in db.FS/migrations, ordered by
// the leading integer in the filename. Each file runs in one transaction
// together with the insert of its schema_migrations row.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	files, err := migrationFiles(db.FS)
	if err != nil {
		return err
	}

	applied := map[int]bool{}
	rows, err := s.pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}

	for _, f := range files {
		if applied[f.version] {
			continue
		}
		sql, err := fs.ReadFile(db.FS, f.path)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", f.path, err)
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("store: begin migration %s: %w", f.path, err)
		}
		// Exec with no arguments uses the simple protocol, so multi-statement
		// migration files work.
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: apply migration %s: %w", f.path, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, f.version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: record migration %s: %w", f.path, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("store: commit migration %s: %w", f.path, err)
		}
	}
	return nil
}

type migrationFile struct {
	version int
	path    string
}

// migrationFiles lists migrations/NNNN_*.sql in fsys ordered by the leading
// integer NNNN.
func migrationFiles(fsys fs.FS) ([]migrationFile, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations dir: %w", err)
	}
	var files []migrationFile
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		head, _, ok := strings.Cut(name, "_")
		if !ok {
			head = strings.TrimSuffix(name, ".sql")
		}
		v, err := strconv.Atoi(head)
		if err != nil {
			return nil, fmt.Errorf("store: migration %q has no leading integer version", name)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("store: duplicate migration version %d (%s, %s)", v, prev, name)
		}
		seen[v] = name
		files = append(files, migrationFile{version: v, path: "migrations/" + name})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files, nil
}
