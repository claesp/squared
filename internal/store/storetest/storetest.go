// Package storetest gives tests a store with an empty database.
//
// Set SQUARED_TEST_DATABASE_URL to a PostgreSQL server where the user can
// make databases, for example with "make test-db". Tests that need a
// database are skipped if the variable is not set.
package storetest

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"squared/internal/store"
)

// New returns a store with a new database that has all migrations. The
// database is removed when the test ends.
func New(t testing.TB) *store.Store {
	t.Helper()
	base := os.Getenv("SQUARED_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SQUARED_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to test database server: %v", err)
	}
	name := "test_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("make test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("remove test database: %v", err)
		}
		_ = admin.Close(context.Background())
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse SQUARED_TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	st, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, Logger()); err != nil {
		t.Fatal(err)
	}
	return st
}

// Logger returns a logger that writes nothing.
func Logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Must stops the test if err is not nil.
func Must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(fmt.Sprint(err))
	}
}
