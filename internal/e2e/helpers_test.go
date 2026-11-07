// Package e2e holds integration tests that need Postgres (TEST_DATABASE_URL)
// and, for the full pipeline test, network access to npm/PyPI (E2E_NETWORK=1).
//
//	TEST_DATABASE_URL=postgres://postgres@127.0.0.1:5432/mcpkg_test?sslmode=disable go test ./internal/e2e -v
package e2e

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/mcpkg/mcpkg/internal/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run database integration tests")
	}
	ctx := context.Background()
	conn, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	// Fresh schema for every test.
	if _, err := conn.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func quietLog() *slog.Logger {
	if os.Getenv("E2E_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
