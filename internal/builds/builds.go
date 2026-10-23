// Package builds is the build lifecycle: a receipt becomes a build, the
// build's sandbox runs, the artifacts land under the receipt's tenant
// prefix. press records the lifecycle and enforces its one rule — a build
// may only move forward — because a state machine that accepts a backward
// move lets a finished build be silently rebuilt under a changed key.
package builds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anywaye/press/internal/store"
)

// State is where a build is. The order is the lifecycle; there is no
// transition that skips a state or walks one back.
type State string

const (
	// StateQueued is recorded, not started. The worker pool picks it up.
	StateQueued State = "queued"
	// StateRunning means a sandbox holds this build.
	StateRunning State = "running"
	// StateSucceeded means the artifacts are written under the tenant
	// prefix. It is terminal.
	StateSucceeded State = "succeeded"
	// StateFailed means the build stopped without artifacts. It is
	// terminal.
	StateFailed State = "failed"
)

// Terminal reports whether the state accepts no transitions.
func (s State) Terminal() bool { return s == StateSucceeded || s == StateFailed }

// Legal reports whether a transition from s to next is allowed. The table is
// total over the four states: an unlisted move is refused, and it is refined
// by the transition's checks in Record.
func (s State) Legal(next State) bool {
	switch s {
	case StateQueued:
		return next == StateRunning || next == StateFailed
	case StateRunning:
		return next == StateSucceeded || next == StateFailed
	case StateSucceeded, StateFailed:
		return false
	}
	return false
}

// ErrIllegalTransition is the one error a caller can act on: the build moved
// in a direction its record does not allow.
var ErrIllegalTransition = errors.New("builds: illegal state transition")

// Build is the record. Tenant is the receipt's tenant and therefore the
// receipt's caller — never a field of its own to set.
type Build struct {
	ID        string
	Tenant    string
	Kind      store.Kind
	ReceiptID string
	State     State
	// ArtifactPrefix is computed at creation from the tenant and kind, so
	// the record itself carries where the artifacts landed and no caller
	// has to remember.
	ArtifactPrefix string
	CreatedAt      time.Time
}

// New creates a queued build for a receipt, with its artifact prefix fixed
// at creation. A build without its prefix is a build whose output location
// is decided later by someone else.
func New(id, tenant string, kind store.Kind, receiptID string, now time.Time) (Build, error) {
	if !kind.Valid() {
		return Build{}, fmt.Errorf("builds: kind %q is not a kind press builds", kind)
	}
	prefix, err := store.ArtifactPrefix(tenant, kind)
	if err != nil {
		return Build{}, fmt.Errorf("builds: %w", err)
	}
	return Build{
		ID:             id,
		Tenant:         tenant,
		Kind:           kind,
		ReceiptID:      receiptID,
		State:          StateQueued,
		ArtifactPrefix: prefix,
		CreatedAt:      now,
	}, nil
}

// Transition applies next to b, enforcing the lifecycle. It is a method on
// the record, not on the store, so the rule holds wherever a build is
// handled — the worker, the API, a test.
func (b *Build) Transition(next State) error {
	if !b.State.Legal(next) {
		return fmt.Errorf("%w: %s -> %s (build %s)", ErrIllegalTransition, b.State, next, b.ID)
	}
	b.State = next
	return nil
}

// Records is the build table's behavior: remember a build, update its state,
// never move one backwards. The interface exists so the lifecycle is tested
// without Postgres.
type Records interface {
	Insert(ctx context.Context, b Build) error
	Transition(ctx context.Context, id string, next State) error
	Get(ctx context.Context, id string) (Build, error)
}
