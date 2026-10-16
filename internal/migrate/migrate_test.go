package migrate

// THE GATED TESTS HERE RUN AGAINST A REAL POSTGRES, IN A DATABASE OF THEIR
// OWN — never against TEST_DATABASE_URL's database itself. What this file
// asserts — that the migrations apply to an EMPTY database, and that a second
// run is a no-op — are properties of a database that starts with no schema
// and no `goose_db_version` table. The scratch database is created and dropped
// by the test through the same DSN's credentials. Without TEST_DATABASE_URL
// every test here skips: the suite must not depend on a database, and a test
// that pretends to be an offline test when it is not is worse than a skip.
//
// The offline contract — refusing with no DSN — runs everywhere, because it
// needs no server at all.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"testing"

	"github.com/cafaye/press/migrations"
)

// TestRunRefusesWithNoDSN is the contract with whatever starts the migrate
// step: a run without a database must fail with a sentence, not start a
// connection to nowhere or report success by doing nothing.
func TestRunRefusesWithNoDSN(t *testing.T) {
	for _, dsn := range []string{"", "   ", "\t"} {
		if _, err := Run(context.Background(), dsn, migrations.FS, testLogger(t)); !errors.Is(err, ErrNoDSN) {
			t.Errorf("Run(%q) returned %v; want ErrNoDSN", dsn, err)
		}
	}
}

// TestAnEmptyDatabaseGrowsTheTablesAndASecondRunIsANoop is the whole deploy
// contract: the first apply creates what the service reads and writes, and
// the re-run the substrate performs on every image change applies nothing.
func TestAnEmptyDatabaseGrowsTheTablesAndASecondRunIsANoop(t *testing.T) {
	dsn := newScratchDatabase(t)
	ctx := context.Background()

	first, err := Run(ctx, dsn, migrations.FS, testLogger(t))
	if err != nil {
		t.Fatalf("applying the embedded migrations to an empty database: %v", err)
	}
	// The control in the other direction: a first run that applied NOTHING
	// would make the no-op assertion below true for the worst reason.
	if len(first) == 0 {
		t.Fatal("the first run against an empty database applied nothing, so the " +
			"`second run applies nothing` assertion below is vacuous")
	}

	for _, table := range []string{"ingest_receipts", "builds"} {
		if !tableExists(t, dsn, table) {
			t.Errorf("table %s does not exist after migrating an empty database", table)
		}
	}

	second, err := Run(ctx, dsn, migrations.FS, testLogger(t))
	if err != nil {
		t.Fatalf("the second run failed: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("a second run against the same database applied %v; the deploy's "+
			"re-run depends on this being empty", second)
	}
}

// newScratchDatabase creates a database for one test and returns a DSN for
// it. A DATABASE AND NOT A SCHEMA: production is a database of its own, and a
// private `search_path` would be a second, quieter schema model production
// does not have. The database is dropped on the way out.
//
// It SKIPS rather than fails when there is no server or no CREATEDB: the
// suite must stay green with no database, and a red here would read like a
// code failure when it is really a missing server.
func newScratchDatabase(t *testing.T) string {
	t.Helper()

	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skipf("TEST_DATABASE_URL is not set, so there is no server to migrate against")
	}

	ctx := context.Background()
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Skipf("opening an admin connection to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("TEST_DATABASE_URL is set but not reachable (%v). Is the server up?", err)
	}

	name := "press_migrate_" + randomSuffix(t)

	if _, err := admin.ExecContext(ctx, `create database "`+name+`"`); err != nil {
		t.Skipf("creating the scratch database %s (does TEST_DATABASE_URL's user hold CREATEDB?): %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `drop database if exists "`+name+`"`); err != nil {
			t.Errorf("dropping the scratch database %s: %v", name, err)
		}
	})

	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parsing TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

func randomSuffix(t *testing.T) string {
	t.Helper()

	var noise [6]byte
	if _, err := rand.Read(noise[:]); err != nil {
		t.Fatalf("generating a database name: %v", err)
	}
	return hex.EncodeToString(noise[:])
}

// tableExists asks the database, not the error, whether a migration ran.
func tableExists(t *testing.T, dsn, name string) bool {
	t.Helper()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening the scratch database: %v", err)
	}
	defer func() { _ = db.Close() }()

	var exists bool
	if err := db.QueryRowContext(context.Background(),
		`select exists (select 1 from information_schema.tables
		                where table_schema = 'public' and table_name = $1)`, name).Scan(&exists); err != nil {
		t.Fatalf("asking the database about %s: %v", name, err)
	}
	return exists
}

// testLogger routes goose's own progress lines into the test's output
// (visible under -v).
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(&testWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	if line := bytes.TrimRight(p, "\n"); len(line) > 0 {
		w.t.Logf("%s", line)
	}
	return len(p), nil
}
