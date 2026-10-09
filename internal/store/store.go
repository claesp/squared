// Package store keeps the data of Squared in PostgreSQL.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"squared/internal/domain"
)

//go:embed migrations/*.sql
var migrations embed.FS

// querier is the part of the pgx API that both a pool and a transaction
// supply.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store gives access to the database. A Store that InTx gives to its
// function uses one transaction.
type Store struct {
	db   querier
	pool *pgxpool.Pool // nil in a transaction
}

// Open connects to the database at url.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	// The default pool size is the number of CPUs. The worker holds one
	// connection for each task lock and needs more for the queries of the
	// task, thus use a larger pool. The URL parameter pool_max_conns
	// changes the value.
	if !strings.Contains(url, "pool_max_conns") {
		cfg.MaxConns = 20
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &Store{db: pool, pool: pool}, nil
}

// Close closes all connections.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Ping checks the connection to the database.
func (s *Store) Ping(ctx context.Context) error {
	if s.pool == nil {
		return nil
	}
	return s.pool.Ping(ctx)
}

// InTx runs fn in a transaction. If fn returns an error, InTx rolls back
// the transaction. If s is already in a transaction, fn uses it.
func (s *Store) InTx(ctx context.Context, fn func(tx *Store) error) error {
	if s.pool == nil {
		return fn(s)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(&Store{db: tx})
	})
}

// WithLock runs fn while it holds a PostgreSQL advisory lock for key. Other
// processes that ask for the same key wait. The worker uses it to process
// only one task for each tenant or deployment at the same time, also with
// more than one worker replica.
func (s *Store) WithLock(ctx context.Context, key string, fn func() error) error {
	if s.pool == nil {
		return errors.New("lock: store is in a transaction")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("lock %s: %w", key, err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", key); err != nil {
		return fmt.Errorf("lock %s: %w", key, err)
	}
	defer func() {
		// Use a new context: the lock must be released also if ctx is
		// done. If the release fails, close the connection, which
		// releases the lock.
		if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtextextended($1, 0))", key); err != nil {
			_ = conn.Conn().Close(context.Background())
		}
	}()
	return fn()
}

// mapErr changes database errors into domain errors.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", domain.ErrConflict, pgErr.ConstraintName)
		case "22P02": // invalid_text_representation, for example a bad UUID
			return domain.ErrNotFound
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", domain.ErrConflict, pgErr.ConstraintName)
		}
	}
	return err
}

// nullable changes "" into NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// migrationLockID is a random constant that identifies the advisory lock
// that protects migrations.
const migrationLockID = 7_312_004_551

// Migrate applies all migrations that the database does not have. It is
// safe to call from more than one process at the same time.
func (s *Store) Migrate(ctx context.Context, log *slog.Logger) error {
	if s.pool == nil {
		return errors.New("migrate: store is in a transaction")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}
	defer func() {
		// Use a new context: the lock must be released also if ctx is done.
		if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
			log.Error("release migration lock", "error", err)
		}
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("migrate: create table: %w", err)
	}

	var current int
	if err := conn.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("migrate: read version: %w", err)
	}

	files, err := migrationFiles()
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.version <= current {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + f.name)
		if err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", f.version)
			return err
		})
		if err != nil {
			return fmt.Errorf("migrate: apply %s: %w", f.name, err)
		}
		log.Info("applied migration", "file", f.name)
	}
	return nil
}

type migrationFile struct {
	version int
	name    string
}

// migrationFiles returns the embedded migrations in order. A file name
// starts with its version number, for example "0001_init.sql".
func migrationFiles() ([]migrationFile, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	var files []migrationFile
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			return nil, fmt.Errorf("migrate: bad file name %q", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migrate: bad file name %q", e.Name())
		}
		files = append(files, migrationFile{version: v, name: e.Name()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	for i := 1; i < len(files); i++ {
		if files[i].version == files[i-1].version {
			return nil, fmt.Errorf("migrate: two files have version %d", files[i].version)
		}
	}
	return files, nil
}
