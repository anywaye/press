package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/anywaye/press/internal/builds"
	"github.com/anywaye/press/internal/ingest"
	"github.com/anywaye/press/internal/store"
)

// QueueOnBuilds adapts the builds store to the ingest path's Queue: an
// ingest enqueues exactly one thing, a queued build for its receipt with the
// artifact prefix computed from the tenant and kind. The receipt is durable
// before this runs and the two writes are separate statements on purpose —
// the reconciliation run exists to sweep a receipt whose build never
// enqueued, and a transaction that spanned both would hide a crash window
// instead of naming it.
type QueueOnBuilds struct {
	NewID func() string
	Insert func(ctx context.Context, b builds.Build) error
	Now func() time.Time
}

// Enqueue writes the queued build for a receipt.
func (q QueueOnBuilds) Enqueue(ctx context.Context, r ingest.Receipt) error {
	kind := store.Kind(r.Kind)
	b, err := builds.New(q.NewID(), r.Tenant, kind, r.ID, q.Now())
	if err != nil {
		return err
	}
	return q.Insert(ctx, b)
}

// DefaultBuildID is a 128-bit server-side id, the same shape the receipt id
// takes and for the same reason: the id is half of a key nobody outside
// press chooses.
func DefaultBuildID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("ingest: cannot mint a build id: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// ProductionQueue is what the binary wires: the builds store, the default id
// minter, the wall clock.
func Production(insert func(ctx context.Context, b builds.Build) error) QueueOnBuilds {
	return QueueOnBuilds{NewID: DefaultBuildID, Insert: insert, Now: time.Now}
}
