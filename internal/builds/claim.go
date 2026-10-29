package builds

import (
	"context"
	"log/slog"
	"time"
)

// Claimer is what the loop polls. PostgresBuilds implements it against the
// table; tests stand a fake behind it that models the one guarantee the
// loop depends on: a queued build is handed out once.
type Claimer interface {
	Claim(ctx context.Context) (Claimed, error)
}

// Loop polls the claimer and runs each claimed build to a terminal state.
type Loop struct {
	Claims    Claimer
	Work      *Worker
	EngineRef string
	Interval  time.Duration
	Log       *slog.Logger
}

// Once claims a single build and runs it. It reports whether there was a
// build to run; the error is the run's failure reason when the build
// failed, already recorded on the row. An empty queue is (false, nil).
func (l *Loop) Once(ctx context.Context) (bool, error) {
	claimed, err := l.Claims.Claim(ctx)
	if err != nil {
		return false, err
	}
	if !claimed.Found {
		return false, nil
	}
	l.log().Info("press-claim: claimed",
		"build", claimed.Build.ID, "tenant", claimed.Build.Tenant, "kind", claimed.Build.Kind)
	state, err := l.Work.RunClaimed(ctx, claimed.Build, claimed.Receipt, l.EngineRef)
	if err != nil {
		l.log().Info("press-claim: build failed",
			"build", claimed.Build.ID, "state", state, "error", err)
		return true, err
	}
	l.log().Info("press-claim: build finished",
		"build", claimed.Build.ID, "state", state)
	return true, nil
}

// Run polls until the context ends. A claim error (the database away, the
// receipt row missing) is logged and the loop waits out the interval rather
// than spinning: the failure is environmental and the next poll may find
// the world repaired. A build failure never reaches Run as an exit — Once
// already recorded it — so the only way out is ctx.Done, which is SIGTERM
// in the binary.
func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(l.interval())
	defer t.Stop()
	for {
		if _, err := l.Once(ctx); err != nil {
			l.log().Info("press-claim: poll failed, waiting out the interval", "error", err)
		}
		select {
		case <-ctx.Done():
			l.log().Info("press-claim: stopping")
			return
		case <-t.C:
		}
	}
}

func (l *Loop) interval() time.Duration {
	if l.Interval <= 0 {
		return 5 * time.Second
	}
	return l.Interval
}

func (l *Loop) log() *slog.Logger {
	if l.Log == nil {
		return slog.Default()
	}
	return l.Log
}
