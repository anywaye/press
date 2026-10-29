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
		if len(allowed) == 0 {
			slog.Warn("press-gate: PRESS_NOTES_ACCOUNTS is empty — the gate is a closed door")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
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
