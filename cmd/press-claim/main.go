// Command press-claim is the orchestration spine's poller: it claims queued
// builds and runs each through the worker to a terminal state. One process,
// one loop, no scheduler — the fleet runs one of these per build lane, and
// the claim statement is what keeps two lanes from taking the same build.
//
// Separate from the serving path on purpose: claiming is polling, serving
// is request-driven, and a process that does both is a process whose poll
// interval is at the mercy of its request latency. It exits only on SIGTERM
// (or SIGINT); a failed build never stops it, because the build's failure
// is already recorded on its row and the queue behind it still needs
// running.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cafaye/press/internal/builds"
	"github.com/cafaye/press/internal/platform/db"
	"github.com/cafaye/press/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	log := slog.Default()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		// The loop without a database is a poller that can never claim;
		// refuse at startup rather than logging the same connection error
		// every interval forever.
		slog.Error("press-claim: DATABASE_URL is not set; there is no queue to poll")
		os.Exit(1)
	}
	repo := os.Getenv("PRESS_ENGINE_REPO")
	if repo == "" {
		slog.Error("press-claim: PRESS_ENGINE_REPO is not set; there is no engine to build against")
		os.Exit(1)
	}
	ref := envOr("PRESS_ENGINE_REF", "")
	if ref == "" {
		slog.Error("press-claim: PRESS_ENGINE_REF is not set; refusing an unpinned engine")
		os.Exit(1)
	}

	// One store, one vocabulary: PRESS_STORE_* is what internal/config reads
	// and what the deployment's secret provides. This binary used to read
	// PRESS_R2_* — a second name the secret never set — so the claim loop
	// exited at startup while the ingest binary beside it was fine. Found by
	// deploying: the two pods, one store, one env map.
	r2, err := store.NewR2(
		os.Getenv("PRESS_STORE_ENDPOINT"),
		os.Getenv("PRESS_STORE_BUCKET"),
		os.Getenv("PRESS_STORE_ACCESS_KEY_ID"),
		os.Getenv("PRESS_STORE_SECRET_ACCESS_KEY"),
	)
	if err != nil {
		slog.Error("press-claim: object store is not configured", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		slog.Error("press-claim: cannot reach the press database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	records := builds.NewPostgresBuilds(pool)
	worker := builds.NewWorker(
		r2,
		builds.TarExtractor{},
		builds.NewGitEngine(repo),
		builds.NewSubprocessRunner(),
		builds.NewDirUploader(r2),
		records,
	)
	loop := &builds.Loop{
		Claims:    records,
		Work:      worker,
		EngineRef: ref,
		Interval:  pollInterval(),
		Log:       log,
	}

	slog.Info("press-claim: polling", "interval", loop.Interval.String(), "engine_ref", ref)
	loop.Run(ctx)
}

func pollInterval() time.Duration {
	// The poll interval is an env var because the lane's latency budget is
	// an operator's decision, not the code's: five seconds idles cheaply
	// against an empty queue, and a busier lane turns the same knob.
	if raw := os.Getenv("PRESS_POLL_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
		slog.Warn("press-claim: PRESS_POLL_INTERVAL does not parse as a positive duration; using 5s", "value", raw)
	}
	return 5 * time.Second
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
