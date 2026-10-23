// Command press-gate is the private notes site's door: one endpoint that
// nginx's auth_request subrequests before serving any /notes/*
// request. It resolves the caller's bearer token through identity — JWTs
// against identity's key set (with an audience check), opaque session
// tokens through identity's introspection route — and admits only accounts
// on this tenant's allowlist.
//
// Separate from the ingest binary on purpose: the ingest path is written for
// throughput and the door is on the path of every private page view, so a
// slow identity is a slow site and never an open one.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/anywaye/press/internal/auth"
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

// verifierAdapter exposes the chain verifier to the gate as an account id.
type verifierAdapter struct {
	inner auth.Verifier
}

func (a verifierAdapter) Verify(ctx context.Context, token string) (string, error) {
	claims, err := a.inner.Verify(ctx, token)
	if err != nil {
		return "", err
	}
	return claims.AccountID, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	listen := envOr("PRESS_GATE_ADDR", ":8081")
	identityURL := envOr("PRESS_IDENTITY_URL", "https://auth.anywaye.com")
	jwksURL := envOr("PRESS_JWKS_URL", "https://auth.anywaye.com/.well-known/jwks.json")
	audience := envOr("PRESS_NOTES_AUDIENCE", "notes")
	allowed := splitCSV(os.Getenv("PRESS_NOTES_ACCOUNTS"))

	// The verifier: JWTs via identity's key set (audience `notes`), opaque
	// tokens via identity's introspection route — the same route the ingest
	// binary's chain verifier lives on. A caller holds whichever credential
	// their login minted, and both name the tenant.
	jwks, err := auth.NewJWKSVerifierFor(jwksURL, "https://auth.anywaye.com", audience)
	if err != nil {
		slog.Error("press-gate: jwks verifier", "error", err)
		os.Exit(1)
	}
	verifier := auth.ChainVerifier{JWT: jwks, Introspector: introspectionAdapter{
		inner: verify.NewHTTPIntrospector(identityURL),
	}}
	gate := verify.NewGate(verifierAdapter{inner: verifier}, allowed)

	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	http.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		// The gate is ready when it is configured: an empty allowlist is a
		// closed door, which is safe but wrong, and this is where it says so.
		if len(allowed) == 0 {
			slog.Warn("press-gate: PRESS_NOTES_ACCOUNTS is empty — the gate is a closed door")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// The verify endpoint. nginx calls it for every /notes/* request
	// with the original Authorization header attached.
	http.HandleFunc("/verify", gate.Admit)

	slog.Info("press-gate: listening", "addr", listen, "identity", identityURL, "jwks", jwksURL, "audience", audience, "allowed_accounts", len(allowed))
	if err := http.ListenAndServe(listen, nil); err != nil {
		slog.Error("press-gate: exit", "error", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
