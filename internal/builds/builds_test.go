package builds

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cafaye/press/internal/store"
)

func TestNewBuildFixesItsArtifactPrefixAtCreation(t *testing.T) {
	// The prefix is where the artifacts will land. Deciding it later is how
	// a build ends up writing under someone else's tenant.
	b, err := New("b1", "acme", store.KindNotes, "r1", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if want := "tenants/acme/dist/notes/"; b.ArtifactPrefix != want {
		t.Fatalf("artifact prefix = %q, want %q", b.ArtifactPrefix, want)
	}
	if b.State != StateQueued {
		t.Fatalf("state = %q, want queued", b.State)
	}
	if b.Tenant != "acme" {
		t.Fatal("the build's tenant must be the receipt's tenant")
	}
}

func TestTransitionAllowsOnlyForwardMoves(t *testing.T) {
	b, _ := New("b1", "acme", store.KindDocs, "r1", time.Unix(0, 0))
	if err := b.Transition(StateRunning); err != nil {
		t.Fatalf("queued -> running: %v", err)
	}
	if err := b.Transition(StateSucceeded); err != nil {
		t.Fatalf("running -> succeeded: %v", err)
	}
	if err := b.Transition(StateRunning); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("succeeded -> running: err = %v, want the illegal-transition error", err)
	}
	if err := b.Transition(StateSucceeded); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("succeeded -> succeeded: err = %v, want the illegal-transition error", err)
	}
}

func TestTransitionRefusesSkippingAState(t *testing.T) {
	b, _ := New("b1", "acme", store.KindDocs, "r1", time.Unix(0, 0))
	if err := b.Transition(StateSucceeded); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("queued -> succeeded: err = %v, want a refusal", err)
	}
}

func TestFailedBuildCanStillFailAgainConsistently(t *testing.T) {
	// A terminal state is terminal: a failed build that is retried is a NEW
	// build record, not a resurrection of the old one.
	b, _ := New("b1", "acme", store.KindDocs, "r1", time.Unix(0, 0))
	if err := b.Transition(StateFailed); err != nil {
		t.Fatalf("queued -> failed: %v", err)
	}
	if err := b.Transition(StateFailed); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("failed -> failed: err = %v, want a refusal", err)
	}
}

// recordsFake is the smallest Records that can lie, so the persistence
// behavior is testable without the cluster.
type recordsFake struct {
	rows map[string]Build
	bad  map[string]bool
}

func newRecordsFake() *recordsFake {
	return &recordsFake{rows: map[string]Build{}, bad: map[string]bool{}}
}

func (r *recordsFake) Insert(_ context.Context, b Build) error {
	if _, exists := r.rows[b.ID]; exists {
		return errors.New("builds: build id already exists")
	}
	r.rows[b.ID] = b
	return nil
}

func (r *recordsFake) Get(_ context.Context, id string) (Build, error) {
	b, ok := r.rows[id]
	if !ok {
		return Build{}, errors.New("builds: no such build")
	}
	return b, nil
}

func (r *recordsFake) Transition(_ context.Context, id string, next State) error {
	b, ok := r.rows[id]
	if !ok {
		return errors.New("builds: no such build")
	}
	// A real store reads the record, applies the transition, and writes it
	// back — which is only legal as one step, because the lifecycle is
	// checked against the CURRENT state. The fake models that, because a
	// fake that transitions a copy would pass against a store that refuses
	// to persist, and that is the bug this fake exists to catch.
	if err := b.Transition(next); err != nil {
		return err
	}
	r.rows[id] = b
	return nil
}

func TestRecordsTransitionIsGatedByTheLifecycle(t *testing.T) {
	// The same rule through the persistence seam: a driver that tried to
	// walk a succeeded build backwards fails, and the stored record is
	// unchanged.
	r := newRecordsFake()
	b, _ := New("b1", "acme", store.KindNotes, "r1", time.Unix(0, 0))
	if err := r.Insert(context.Background(), b); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := r.Transition(context.Background(), "b1", StateRunning); err != nil {
		t.Fatalf("queued -> running: %v", err)
	}
	if err := r.Transition(context.Background(), "b1", StateSucceeded); err != nil {
		t.Fatalf("running -> succeeded: %v", err)
	}
	if err := r.Transition(context.Background(), "b1", StateRunning); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("succeeded -> running: err = %v, want the illegal-transition error", err)
	}
	stored, _ := r.Get(context.Background(), "b1")
	if stored.State != StateSucceeded {
		t.Fatalf("stored state = %q after a refused transition", stored.State)
	}
}
