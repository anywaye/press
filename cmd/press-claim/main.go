package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/anywaye/press/internal/builds"
	"github.com/anywaye/press/internal/platform/db"
	"github.com/anywaye/press/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	log := slog.Default()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
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

	r2, err := store.NewR2WithToken(
		os.Getenv("PRESS_STORE_ENDPOINT"),
		os.Getenv("PRESS_STORE_BUCKET"),
		os.Getenv("PRESS_STORE_ACCESS_KEY_ID"),
		os.Getenv("PRESS_STORE_SECRET_ACCESS_KEY"),
		os.Getenv("PRESS_STORE_SESSION_TOKEN"),
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
