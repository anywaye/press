package migrate


import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"testing"

	"github.com/anywaye/press/internal/platform/dbtest"
	"github.com/anywaye/press/migrations"
)

func TestRunRefusesWithNoDSN(t *testing.T) {
	for _, dsn := range []string{"", "   ", "\t"} {
		if _, err := Run(context.Background(), dsn, migrations.FS, testLogger(t)); !errors.Is(err, ErrNoDSN) {
			t.Errorf("Run(%q) returned %v; want ErrNoDSN", dsn, err)
		}
	}
}

func TestAnEmptyDatabaseGrowsTheTablesAndASecondRunIsANoop(t *testing.T) {
	dsn := dbtest.ScratchDatabase(t, "press_migrate_")
	ctx := context.Background()

	first, err := Run(ctx, dsn, migrations.FS, testLogger(t))
	if err != nil {
		t.Fatalf("applying the embedded migrations to an empty database: %v", err)
	}
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
