package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config is the resolved configuration. Construct with Load, which fails on a
// value that would make the service misbehave rather than defaulting it.
type Config struct {
	Addr string

	DatabaseURL string

	JWKSURL string
	IdentityURL string

	Store Store

	MaxIngestBytes int64

	BuildConcurrency int
}

// Store is the object storage press writes artifacts to.
type Store struct {
	Endpoint string
	Bucket string
	AccessKeyID     string
	SecretAccessKey string
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

	c.JWKSURL = envOr("PRESS_JWKS_URL", "https://auth.anywaye.com/.well-known/jwks.json")
	c.IdentityURL = envOr("PRESS_IDENTITY_URL", "https://auth.anywaye.com")

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
	if c.Store.Bucket == "" {
		c.Store.Bucket = "anywaye-docs-artifacts"
	}

	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
