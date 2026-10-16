// Package config is press's environment: everything the binary needs that is
// not a request. Every field is read from the environment exactly once at
// startup, because a config value that can change under a running process is a
// config value nobody can reason about. Nothing here has a default that
// changes behavior silently — where a default exists it is the safe one
// (a small ingest cap, the public identity issuer).
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config is the resolved configuration. Construct with Load, which fails on a
// value that would make the service misbehave rather than defaulting it.
type Config struct {
	// Addr is the listen address. The fleet convention is :8080.
	Addr string

	// DatabaseURL is the one Postgres cluster's DSN for the press database.
	// press shares the cluster with identity, billing and courier and owns
	// exactly one database on it, named after itself — the boundary is the
	// database, not the machine (architecture/one-cluster).
	DatabaseURL string

	// JWKSURL is identity's public key set, where caller tokens are verified.
	JWKSURL string

	// Store is the R2 (S3 API) bucket press ingests into and builds write to.
	Store Store

	// MaxIngestBytes is the largest tarball press will accept in one request.
	// The cap is enforced while streaming, before anything is written, so an
	// over-cap request leaves no partial object behind.
	MaxIngestBytes int64

	// BuildConcurrency is how many builds press will run at once. One build
	// is a sandbox with a node toolchain; the substrate is shared.
	BuildConcurrency int
}

// Store is the object storage press writes artifacts to.
type Store struct {
	// Endpoint is the S3 API origin, e.g.
	// https://<account>.r2.cloudflarestorage.com. Empty means the AWS
	// default, which is not where cafaye's objects live.
	Endpoint string
	// Bucket is the artifact bucket. Every press object lives under it,
	// tenant-scoped by key.
	Bucket string
	// AccessKeyID and SecretAccessKey are the R2 credentials.
	AccessKeyID     string
	SecretAccessKey string
	// SessionToken is present when the credentials are R2's temporary
	// (STS-style) credentials derived from a parent R2 API token — which is
	// the fleet's shape: a long-lived parent on the box, short-lived derived
	// credentials, refreshed on a schedule.
	SessionToken string
}

// DefaultMaxIngestBytes is 64 MiB: a documentation tarball is markdown and
// the occasional diagram, and a cap that generous is a cap nothing legitimate
// hits while a runaway client still cannot fill the disk.
const DefaultMaxIngestBytes = 64 << 20

// Load reads the environment and fails with the variable named.
func Load() (Config, error) {
	var c Config
	var err error

	c.Addr = envOr("PRESS_ADDR", ":8080")
	c.DatabaseURL = os.Getenv("DATABASE_URL")
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required: press records every ingest in the press database on the one cluster")
	}

	c.JWKSURL = envOr("PRESS_JWKS_URL", "https://auth.cafaye.com/.well-known/jwks.json")

	c.MaxIngestBytes = DefaultMaxIngestBytes
	if v := os.Getenv("PRESS_MAX_INGEST_BYTES"); v != "" {
		c.MaxIngestBytes, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			return c, fmt.Errorf("PRESS_MAX_INGEST_BYTES is not a byte count: %w", err)
		}
		if c.MaxIngestBytes <= 0 {
			return c, fmt.Errorf("PRESS_MAX_INGEST_BYTES must be positive, got %d", c.MaxIngestBytes)
		}
	}

	c.BuildConcurrency = 1
	if v := os.Getenv("PRESS_BUILD_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("PRESS_BUILD_CONCURRENCY must be a positive integer, got %q", v)
		}
		c.BuildConcurrency = n
	}

	c.Store = Store{
		Endpoint:        os.Getenv("PRESS_STORE_ENDPOINT"),
		Bucket:          os.Getenv("PRESS_STORE_BUCKET"),
		AccessKeyID:     os.Getenv("PRESS_STORE_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("PRESS_STORE_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("PRESS_STORE_SESSION_TOKEN"),
	}
	// The store is not required at boot the way the database is: press
	// answers /healthz and /readyz without it, and the readiness check is
	// what refuses traffic when storage is misconfigured. A boot that fails
	// on a missing bucket is a boot that cannot report *why* it is broken.
	if c.Store.Bucket == "" {
		c.Store.Bucket = "cafaye-docs-artifacts"
	}

	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
