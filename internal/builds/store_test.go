package builds

// THE GATED TESTS HERE RUN AGAINST A REAL POSTGRES, IN A DATABASE OF THEIR
// OWN — never against TEST_DATABASE_URL's database itself. What they assert —
// that the claim round-trips through the real tables, and that two
// simultaneous claims grant one build — needs the real lock manager; no fake
// can hold a lock. Without TEST_DATABASE_URL every test here skips: the suite
// must not depend on a database.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anywaye/press/internal/migrate"
	"github.com/anywaye/press/internal/platform/dbtest"
	"github.com/anywaye/press/internal/store"
	"github.com/anywaye/press/migrations"
)

// openMigratedPool migrates a scratch database and returns a pool on it. The
// migration is part of the setup, not the assertion: store tests assume the
// schema the migrate package owns.
func openMigratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	dsn := dbtest.ScratchDatabase(t, "press_claim_")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := migrate.Run(ctx, dsn, migrations.FS, logger); err != nil {
		t.Fatalf("migrating the scratch database: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedReceiptAndBuild writes the two rows a claim joins: the receipt first,
// because the build's foreign key points at it.
func seedReceiptAndBuild(t *testing.T, pool *pgxpool.Pool, tenant, buildID string) {
	t.Helper()
	ctx := context.Background()

	receiptID := "r-" + buildID
	_, err := pool.Exec(ctx, `
		INSERT INTO ingest_receipts
		    (id, tenant_id, kind, project, version, sha256, size, state, object_key, caller)
		VALUES ($1, $2, 'docs', '', '', 'abc', 3, 'stored', $3, 'user-1')`,
		receiptID, tenant, "tenants/"+tenant+"/ingest/"+receiptID+".tar.gz")
	if err != nil {
		t.Fatalf("seeding the receipt: %v", err)
	}
	b, err := New(buildID, tenant, store.KindDocs, receiptID, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := NewPostgresBuilds(pool).Insert(ctx, b); err != nil {
		t.Fatalf("seeding the build: %v", err)
	}
}

func TestPostgresBuildsRoundTripsTheLifecycle(t *testing.T) {
	// Insert, read back, walk forward — and the backward move is refused with
	// the lifecycle error, through the real table.
	pool := openMigratedPool(t)
	ctx := context.Background()
	recs := NewPostgresBuilds(pool)

	seedReceiptAndBuild(t, pool, "acme", "b1")

	got, err := recs.Get(ctx, "b1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Tenant != "acme" || got.State != StateQueued || got.ArtifactPrefix != "tenants/acme/dist/docs/" {
		t.Fatalf("round trip = %+v; want the queued build with its prefix", got)
	}

	if err := recs.Transition(ctx, "b1", StateRunning); err != nil {
		t.Fatalf("queued -> running: %v", err)
	}
	if err := recs.Transition(ctx, "b1", StateSucceeded); err != nil {
		t.Fatalf("running -> succeeded: %v", err)
	}
	if err := recs.Transition(ctx, "b1", StateRunning); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("succeeded -> running: err = %v, want the illegal-transition refusal", err)
	}
	if _, err := recs.Get(ctx, "missing"); !errors.Is(err, ErrNoSuchBuild) {
		t.Fatalf("get missing: err = %v, want the no-such-build error", err)
	}
}

func TestTwoSimultaneousClaimsGrantOneBuild(t *testing.T) {
	// THE CONCURRENCY EVIDENCE. Two claims run at the same time against one
	// queued build; exactly one of them takes it, and the row is running
	// afterwards. The assertion is deterministic under every interleave:
	// serial or overlapping, the single claim statement can only move
	// queued → running once, and the loser finds no queued row.
	pool := openMigratedPool(t)
	ctx := context.Background()

	seedReceiptAndBuild(t, pool, "acme", "b1")

	recs := NewPostgresBuilds(pool)

	start := make(chan struct{})
	results := make([]Claimed, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = recs.Claim(ctx)
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
	}
	found := 0
	for _, r := range results {
		if r.Found {
			found++
			if r.Build.ID != "b1" || r.Build.State != StateRunning {
				t.Fatalf("claimed = %+v; want build b1, running", r.Build)
			}
			if r.Build.Tenant != "acme" {
				t.Fatalf("claimed tenant = %q; the row keeps its tenant", r.Build.Tenant)
			}
			if want := "tenants/acme/ingest/r-b1.tar.gz"; r.Receipt.ObjectKey != want {
				t.Fatalf("receipt object key = %q, want %q", r.Receipt.ObjectKey, want)
			}
			if r.Receipt.Tenant != "acme" {
				t.Fatalf("receipt tenant = %q; the join must agree with the build", r.Receipt.Tenant)
			}
		}
	}
	if found != 1 {
		t.Fatalf("%d of 2 simultaneous claims took the build, want exactly 1", found)
	}

	// The queue is now empty: a third claim is the idle no-op.
	again, err := recs.Claim(ctx)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if again.Found {
		t.Fatal("a third claim took a build that is already running")
	}

	// The row itself is running: the claim moved it, not just reported it.
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM builds WHERE id = 'b1'`).Scan(&state); err != nil {
		t.Fatalf("reading the claimed row: %v", err)
	}
	if state != "running" {
		t.Fatalf("row state = %q, want running", state)
	}
}
