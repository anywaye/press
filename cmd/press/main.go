// Command press is the documentation service: ingest, build, tenant-isolated
// artifacts. One binary, no other runtime dependency — the same shape as
// every service in the fleet.
//
// The wiring order is the failure order: config, database, verifier, store,
// service. A failure at any step names the step, and the process does not
// start without any of them, because a press that boots with a broken store
// is a press that accepts tarballs it cannot file.
package main

import (
	"context"
	"errors"
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
	"github.com/anywaye/press/internal/ingest"
	"github.com/anywaye/press/internal/platform/db"
	"github.com/anywaye/press/internal/queue"
	"github.com/anywaye/press/internal/store"
	"github.com/anywaye/press/internal/verify"
)

// introspectionAdapter joins the gate's introspector to the chain's interface:
// same route, same answer, two packages that must not import each other's
// types (the gate serves, the chain verifies).
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

	// The database: the press database on the one cluster.
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The verifier: JWTs via identity's key set, opaque session tokens via
	// identity's introspection route — the same route the notes gate lives
	// on. A caller holds whichever credential their login minted, and both
	// name the tenant.
	jwks, err := auth.NewJWKSVerifierFor(cfg.JWKSURL, "https://auth.anywaye.com", "press")
	if err != nil {
		return err
	}
	verifier := auth.ChainVerifier{JWT: jwks, Introspector: introspectionAdapter{
		inner: verify.NewHTTPIntrospector(cfg.IdentityURL),
	}}

	// The store: R2 over its S3 API.
	r2, err := store.NewR2WithToken(cfg.Store.Endpoint, cfg.Store.Bucket, cfg.Store.AccessKeyID, cfg.Store.SecretAccessKey, cfg.Store.SessionToken)
	if err != nil {
		return err
	}

	// The services. The build worker is not wired in this commit: the
	// lifecycle and its record are in place, and the sandbox that runs
	// anywaye/docs's build is the next packet — a stated gap, not an
	// implied one.
	receipts := ingest.NewPostgresReceipts(pool)
	// The queue: the ingest's receipt and the build it exists to feed.
	buildStore := builds.NewPostgresBuilds(pool)
	ingestSvc := ingest.NewService(verifier, r2, receipts, queue.Production(buildStore.Insert), cfg.MaxIngestBytes)

	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Readiness is the database's: press with an unreachable cluster can
	// answer health and cannot file a receipt, and the substrate has to
	// hear the difference.
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
