package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/anywaye/press/internal/auth"
	"github.com/anywaye/press/internal/builds"
	"github.com/anywaye/press/internal/config"
	"github.com/anywaye/press/internal/errorreporting"
	"github.com/anywaye/press/internal/httpapi"
	"github.com/anywaye/press/internal/ingest"
	"github.com/anywaye/press/internal/platform/db"
	"github.com/anywaye/press/internal/queue"
	"github.com/anywaye/press/internal/store"
	"github.com/anywaye/press/internal/telemetry"
	"github.com/anywaye/press/internal/verify"
)

type introspectionAdapter struct {
	inner *verify.HTTPIntrospector
}

func (a introspectionAdapter) Introspect(ctx context.Context, token string) (auth.Introspection, error) {
	in, err := a.inner.Introspect(ctx, token)
	if err != nil {
		return auth.Introspection{}, err
	}
	return auth.Introspection{Active: in.Active, AccountID: in.AccountID}, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("press: exit", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	errorreporting.Init()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	jwks, err := auth.NewJWKSVerifierFor(cfg.JWKSURL, "https://auth.anywaye.com", "press")
	if err != nil {
		return err
	}
	verifier := auth.ChainVerifier{JWT: jwks, Introspector: introspectionAdapter{
		inner: verify.NewHTTPIntrospector(cfg.IdentityURL),
	}}

	r2, err := store.NewR2WithToken(cfg.Store.Endpoint, cfg.Store.Bucket, cfg.Store.AccessKeyID, cfg.Store.SecretAccessKey, cfg.Store.SessionToken)
	if err != nil {
		return err
	}

	receipts := ingest.NewPostgresReceipts(pool)
	buildStore := builds.NewPostgresBuilds(pool)
	ingestSvc := ingest.NewService(verifier, r2, receipts, queue.Production(buildStore.Insert), cfg.MaxIngestBytes)

	var flush telemetry.Shutdown
	flush, err = telemetry.Install(ctx, telemetry.Options{
		Endpoint:  telemetry.Endpoint(),
		Disabled:  !telemetry.Enabled(),
		LookupEnv: os.Getenv,
	})
	if err != nil {
		slog.Error("telemetry install failed", "error", err)
	} else {
		defer func() {
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := flush(flushCtx); err != nil {
				slog.Error("telemetry flush failed", "error", err)
			}
		}()
	}
	if reason := telemetry.NoOpReason(); reason != "" {
		slog.Info("telemetry is off", "reason", reason)
	}

	r := chi.NewRouter()
	r.Use(httpapi.TelemetryMiddleware)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Trace-Id") == "glitchtip-probe" {
			errorreporting.CaptureError(fmt.Errorf("glitchtip probe press"), r)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("probe"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	r.Get("/metrics", promhttp.Handler().ServeHTTP)
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "press database unreachable", http.StatusServiceUnavailable)
			return
		}
		if _, err := store.NewR2(cfg.Store.Endpoint, cfg.Store.Bucket, cfg.Store.AccessKeyID, cfg.Store.SecretAccessKey); err != nil {
			http.Error(w, "artifact store misconfigured", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	r.Mount("/v1/ingest", ingestSvc.Handler())

	handler := errorreporting.RecoverMiddleware(r)
	srv := &http.Server{Addr: cfg.Addr, Handler: handler}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("press: listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("press: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
