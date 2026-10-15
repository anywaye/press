package ingest

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgresReceipts is the receipt table on the one cluster. Every statement
// is tenant-scoped because every row is: an unscoped SELECT would be the one
// query that turns a row-level boundary into a leak.
type PostgresReceipts struct {
	q pgxExecer
}

// pgxExecer is the pgx surface the receipts need, so tests can stand in for
// the pool without a database.
type pgxExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func NewPostgresReceipts(q pgxExecer) *PostgresReceipts {
	return &PostgresReceipts{q: q}
}

// Insert records a receipt. The id was minted server-side and the object
// already exists; a conflict means the id generator collided, which the
// caller sees as an error and not as a silent overwrite.
func (p *PostgresReceipts) Insert(ctx context.Context, r Receipt) error {
	const stmt = `
		INSERT INTO ingest_receipts
		    (id, tenant_id, kind, project, version, sha256, size, state, object_key, caller)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	_, err := p.q.Exec(ctx, stmt,
		r.ID, r.Tenant, r.Kind, r.Project, r.Version, r.SHA256, r.Size, r.State, r.ObjectKey, r.Caller)
	if err != nil {
		return fmt.Errorf("press: record the receipt: %w", err)
	}
	return nil
}
