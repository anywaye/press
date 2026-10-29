package builds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/anywaye/press/internal/store"
)

// ErrNoSuchBuild is what Get and Transition report for an id the table
// never held. It is distinct from ErrIllegalTransition on purpose: "no such
// build" is a wiring bug in the caller, "the move is not allowed" is the
// lifecycle refusing, and the two want different responses.
var ErrNoSuchBuild = errors.New("builds: no such build")

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
