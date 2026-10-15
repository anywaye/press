// Package db is press's half of the fleet's one-Postgres convention: one
// connection pool to the press database on the shared cluster, and nothing
// that reaches another service's database. The boundary is the database —
// identity, billing, courier and press each own one — and the role press
// connects as owns only this one.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open connects and verifies the connection. A pool that cannot ping is a
// press that cannot record a receipt, so this fails at startup rather than
// at the first ingest.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: DATABASE_URL does not parse: %w", err)
	}
	// The stated budget from architecture/one-cluster: small and fixed,
	// because the cluster is shared and an unbounded pool is one service
	// taking the others' connections.
	cfg.MaxConns = 8
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: cannot create the pool: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: cannot reach the press database: %w", err)
	}
	return pool, nil
}
