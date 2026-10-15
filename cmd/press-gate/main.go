// Command press-gate is the private notes site's door: one endpoint that
// traefik's ForwardAuth middleware subrequests before serving any /notes/*
// request. It resolves the caller's bearer token through identity's
// introspection route and admits only accounts on this tenant's allowlist.
//
// Separate from the ingest binary on purpose: the ingest path is written for
// throughput and the door is on the path of every private page view, so a
// slow identity is a slow site and never an open one.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/cafaye/press/internal/verify"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	listen := envOr("PRESS_GATE_ADDR", ":8081")
	identityURL := envOr("PRESS_IDENTITY_URL", "https://auth.cafaye.com")
	allowed := splitCSV(os.Getenv("PRESS_NOTES_ACCOUNTS"))

	gate := verify.NewGate(verify.NewHTTPIntrospector(identityURL), allowed)

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
	// The ForwardAuth endpoint. traefik calls it for every /notes/* request
	// with the original Authorization header attached.
	http.HandleFunc("/verify", gate.Admit)

	slog.Info("press-gate: listening", "addr", listen, "identity", identityURL, "allowed_accounts", len(allowed))
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
