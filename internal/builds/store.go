// The build table on the one cluster: the same pgx shape as
// ingest.PostgresReceipts (an interface over the pool plus a constructor),
// because the fleet reads a store the same way everywhere and a second
// pattern here would be a second thing to audit for tenant scoping.
//
// # WHY THE CLAIM IS ONE STATEMENT
//
// Two press-claim processes poll the same table. A claim that SELECTs a
// queued build and then UPDATEs it is two statements with a gap between
// them, and the gap is where a double-claim lives: both processes read the
// same queued row, both move it to running, both build it, and the tenant
// gets two uploads racing under one prefix. The claim below is a single
// UPDATE over a CTE that locks the chosen row FOR UPDATE and skips rows
// another claimer already holds (SKIP LOCKED), so the database serializes
// the decision and a build is handed to exactly one runner. A double-claim
// would cost a tenant a half-overwritten site; the statement's shape is
// what prevents it.
//
// # WHY THE CLAIM SELECTS BY STATE AND NOT BY TENANT
//
// Every other statement in this service filters by tenant_id, because every
// other statement answers a question about one tenant. The claim answers a
// different question — "is there work anywhere" — and a tenant filter on it
// would idle the loop while another tenant's queue waited. The tenant stays
// on the row (it is written at insert and read back on claim), and the
// worker refuses a receipt whose tenant is not the build's, so the claim's
// breadth never becomes a breadth of writes: the upload target is the
// claimed record's own prefix.
package builds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cafaye/press/internal/store"
)

// ErrNoSuchBuild is what Get and Transition report for an id the table
// never held. It is distinct from ErrIllegalTransition on purpose: "no such
// build" is a wiring bug in the caller, "the move is not allowed" is the
// lifecycle refusing, and the two want different responses.
var ErrNoSuchBuild = errors.New("builds: no such build")

// pgxQuerier is the pgx surface the builds need: Exec for writes, QueryRow
// for the claim and the reads. Tests stand a fake behind it; production
// passes the pool, which implements both.
type pgxQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PostgresBuilds is the builds table. It implements Records, plus Claim for
// the loop.
type PostgresBuilds struct {
	q pgxQuerier
}

// NewPostgresBuilds wires the table over a pool (or a fake in tests).
func NewPostgresBuilds(q pgxQuerier) *PostgresBuilds {
	return &PostgresBuilds{q: q}
}

// Insert records a queued build. The artifact prefix was fixed at New, so
// what is stored is where the artifacts will land — no later step decides.
func (p *PostgresBuilds) Insert(ctx context.Context, b Build) error {
	const stmt = `
		INSERT INTO builds
		    (id, tenant_id, kind, receipt_id, state, artifact_prefix, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	created := b.CreatedAt
	if created.IsZero() {
		// A build without a creation time sorts nowhere in the claim's
		// ORDER BY, so the store stamps one rather than storing the zero
		// time and letting the queue order lie.
		created = time.Now()
	}
	_, err := p.q.Exec(ctx, stmt,
		b.ID, b.Tenant, string(b.Kind), b.ReceiptID, string(b.State), b.ArtifactPrefix, created)
	if err != nil {
		return fmt.Errorf("builds: record the build: %w", err)
	}
	return nil
}

// Get reads one build back.
func (p *PostgresBuilds) Get(ctx context.Context, id string) (Build, error) {
	const stmt = `
		SELECT id, tenant_id, kind, receipt_id, state, artifact_prefix, created_at
		FROM builds
		WHERE id = $1`
	var b Build
	var kind, state string
	if err := p.q.QueryRow(ctx, stmt, id).Scan(
		&b.ID, &b.Tenant, &kind, &b.ReceiptID, &state, &b.ArtifactPrefix, &b.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Build{}, fmt.Errorf("builds: %w: %s", ErrNoSuchBuild, id)
		}
		return Build{}, fmt.Errorf("builds: read the build: %w", err)
	}
	b.Kind, b.State = store.Kind(kind), State(state)
	return b, nil
}

// Transition moves a build forward, enforcing the lifecycle in the database
// rather than trusting the caller checked first. The read names the current
// state so the refusal can say what the move was; the write repeats the
// current state in its WHERE clause, so a concurrent claim that moved the
// row between the read and the write fails the write instead of overwriting
// it — the loser sees ErrIllegalTransition and not a silent second move.
func (p *PostgresBuilds) Transition(ctx context.Context, id string, next State) error {
	current, err := p.Get(ctx, id)
	if err != nil {
		return err
	}
	// The lifecycle check runs against the CURRENT state, the same rule the
	// Build method enforces, so the store and the record never disagree
	// about which moves are legal.
	probe := current
	if err := probe.Transition(next); err != nil {
		return err
	}
	const stmt = `
		UPDATE builds
		SET state = $2
		WHERE id = $1 AND state = $3`
	tag, err := p.q.Exec(ctx, stmt, id, string(next), string(current.State))
	if err != nil {
		return fmt.Errorf("builds: move the build: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Someone moved the row after the read — the claim loop is the
		// usual someone. The move is refused rather than retried, because
		// a retry would apply a transition checked against a state the row
		// no longer holds.
		return fmt.Errorf("%w: %s -> %s (build %s moved under the transition)",
			ErrIllegalTransition, current.State, next, id)
	}
	return nil
}

// Claimed is what the loop holds: the build, now running and owned by this
// process, and the receipt that locates its tarball. The receipt comes from
// the same claim so the loop never builds from a receipt row read
// separately and possibly for another tenant's object. Found reports whether
// there was a build to take — an empty queue is a value, not an error.
type Claimed struct {
	Build   Build
	Receipt ReceiptSource
	Found   bool
}

// Claim takes the oldest queued build and moves it to running in one
// statement, returning it with its receipt. With nothing queued it reports
// Found false and no error — an empty queue is the loop's normal state, not
// a failure.
func (p *PostgresBuilds) Claim(ctx context.Context) (Claimed, error) {
	// One statement: the CTE locks the oldest queued row (SKIP LOCKED steps
	// over rows a concurrent claimer holds), and the UPDATE moves exactly
	// that row to running and hands it back. Two processes running this at
	// once divide the queue between them; neither ever sees the other's
	// row.
	const claim = `
		WITH next AS (
		    SELECT id
		    FROM builds
		    WHERE state = 'queued'
		    ORDER BY created_at ASC, id ASC
		    LIMIT 1
		    FOR UPDATE SKIP LOCKED
		)
		UPDATE builds AS b
		SET state = 'running'
		FROM next
		WHERE b.id = next.id
		RETURNING b.id, b.tenant_id, b.kind, b.receipt_id, b.state, b.artifact_prefix, b.created_at`
	var c Claimed
	var kind, state string
	if err := p.q.QueryRow(ctx, claim).Scan(
		&c.Build.ID, &c.Build.Tenant, &kind, &c.Build.ReceiptID,
		&state, &c.Build.ArtifactPrefix, &c.Build.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Claimed{}, nil
		}
		return Claimed{}, fmt.Errorf("builds: claim a build: %w", err)
	}
	c.Build.Kind, c.Build.State = store.Kind(kind), State(state)

	// The receipt locates the tarball. It is read after the claim because
	// the builds row, not the receipt row, is what concurrent claimers
	// contend on — and the worker refuses a receipt whose tenant is not the
	// build's, so a mismatched row fails the build instead of crossing a
	// tenant boundary.
	const receipt = `
		SELECT tenant_id, kind, object_key
		FROM ingest_receipts
		WHERE id = $1`
	var rTenant, rKind, objectKey string
	if err := p.q.QueryRow(ctx, receipt, c.Build.ReceiptID).Scan(&rTenant, &rKind, &objectKey); err != nil {
		return Claimed{}, fmt.Errorf("builds: read the claimed build's receipt: %w", err)
	}
	c.Receipt = ReceiptSource{
		ID:        c.Build.ReceiptID,
		Tenant:    rTenant,
		Kind:      store.Kind(rKind),
		ObjectKey: objectKey,
	}
	c.Found = true
	return c, nil
}
