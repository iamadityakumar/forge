// Package testutil provides helpers for integration tests that need a real
// PostgreSQL instance without colliding with the running system.
package testutil

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PrepareTestDB returns a DSN for a dedicated, schema-ready test database,
// creating it (and applying migrations/*.up.sql) on first use.
//
// The database is named <base>_test_<suffix> — e.g. `forge` + suffix "store"
// becomes `forge_test_store` — a sibling of the configured database. This keeps
// integration tests off the live `forge` database that a running worker stack
// uses: tests can no longer be disturbed by (or truncate) real jobs, and two
// package test binaries (store/agent/worker) each get their own database so
// they cannot race each other's TRUNCATE either.
//
// When no database is reachable, or the current user may not create databases,
// the test is skipped rather than failed — the same gating the pre-existing
// integration helpers used for a missing Postgres.
func PrepareTestDB(t *testing.T, suffix string) string {
	t.Helper()

	base := os.Getenv("DATABASE_URL")
	if base == "" {
		base = "postgres://postgres:secret@localhost:5432/forge?sslmode=disable"
	}

	u, err := url.Parse(base)
	if err != nil {
		t.Skipf("skipping integration test: invalid DATABASE_URL %q: %v", base, err)
	}

	baseName := strings.TrimPrefix(u.Path, "/")
	if baseName == "" || strings.Contains(baseName, "/") {
		t.Skipf("skipping integration test: cannot determine database name from %q", base)
	}

	testName := sanitizeIdent(baseName) + "_test_" + sanitizeIdent(suffix)

	// CREATE DATABASE cannot run inside a transaction, so drive it over a
	// throwaway connection to the configured database (same server).
	adm, err := sql.Open("pgx", base)
	if err != nil {
		t.Skipf("skipping integration test: cannot open admin connection: %v", err)
	}
	var exists bool
	if err := adm.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, testName).Scan(&exists); err != nil {
		adm.Close()
		t.Skipf("skipping integration test: cannot inspect databases: %v", err)
	}
	if !exists {
		if _, err := adm.Exec(fmt.Sprintf(`CREATE DATABASE "%s"`, testName)); err != nil {
			// A sibling test binary may have created it between the check and the
			// CREATE (packages run in parallel processes). Treat that as success.
			var now bool
			if e2 := adm.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, testName).Scan(&now); e2 != nil || !now {
				adm.Close()
				t.Skipf("skipping integration test: cannot create %s: %v", testName, err)
			}
		}
	}
	adm.Close()

	// Apply the schema the first time the test database is seen.
	testURL := *u
	testURL.Path = "/" + testName
	db, err := sql.Open("pgx", testURL.String())
	if err != nil {
		t.Skipf("skipping integration test: cannot open %s: %v", testName, err)
	}
	defer db.Close()

	var hasJobs bool
	if err := db.QueryRow(`SELECT to_regclass('public.jobs') IS NOT NULL`).Scan(&hasJobs); err != nil {
		t.Skipf("skipping integration test: cannot inspect schema: %v", err)
	}
	if !hasJobs {
		applyMigrations(t, db)
	}

	return testURL.String()
}

// sanitizeIdent reduces an arbitrary string to a safe [a-zA-Z0-9_] identifier.
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// migrationsDir locates the repo-root migrations directory by walking up from
// the test's working directory until it finds go.mod.
func migrationsDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return "migrations"
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "migrations")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join("..", "..", "migrations")
}

func applyMigrations(t *testing.T, db *sql.DB) {
	t.Helper()

	dir := migrationsDir()
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		t.Fatalf("cannot list migrations in %s: %v", dir, err)
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("no migrations found in %s", dir)
	}

	// Apply all migrations in one transaction so a mid-way failure leaves the
	// test database without a half-applied schema.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin migration transaction: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("read migration %s: %v", filepath.Base(f), err)
		}
		// Some migrations carry a UTF-8 BOM (EF BB BF); PostgreSQL rejects it,
		// so strip it before handing the script to the server.
		b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
		if _, err := tx.Exec(string(b)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply migration %s: %v", filepath.Base(f), err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit migrations: %v", err)
	}
}
