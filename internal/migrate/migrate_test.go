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
	"database/sql"
	"errors"
	"log/slog"
	"testing"

	"github.com/cafaye/press/internal/platform/dbtest"
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
	dsn := dbtest.ScratchDatabase(t, "press_migrate_")
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
