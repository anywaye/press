package dbtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// ScratchDatabase creates an empty database for one test and returns a DSN
// for it. A DATABASE AND NOT A SCHEMA: production is a database of its own,
// and a private `search_path` would be a second, quieter schema model
// production does not have. The database is dropped on the way out.
func ScratchDatabase(t *testing.T, prefix string) string {
	t.Helper()

	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skipf("TEST_DATABASE_URL is not set, so there is no server to test against")
	}

	ctx := context.Background()
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Skipf("opening an admin connection to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("TEST_DATABASE_URL is set but not reachable (%v). Is the server up?", err)
	}

	name := prefix + randomSuffix(t)

	if _, err := admin.ExecContext(ctx, `create database "`+name+`"`); err != nil {
		t.Skipf("creating the scratch database %s (does TEST_DATABASE_URL's user hold CREATEDB?): %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `drop database if exists "`+name+`"`); err != nil {
			t.Errorf("dropping the scratch database %s: %v", name, err)
		}
	})

	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parsing TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

func randomSuffix(t *testing.T) string {
	t.Helper()

	var noise [6]byte
	if _, err := rand.Read(noise[:]); err != nil {
		t.Fatalf("generating a database name: %v", err)
	}
	return hex.EncodeToString(noise[:])
}
