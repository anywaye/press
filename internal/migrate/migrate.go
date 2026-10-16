// Package migrate applies this repository's goose migrations from an embedded
// copy of `migrations/`, inside a process that starts, applies, and exits —
// never from the serving path. Migrating stays a deploy step: the substrate
// runs a one-shot migrate Job before the service boots against the schema,
// because a service that boots against an empty schema fails every query with
// `relation does not exist` and says nothing about why.
//
// # WHY THE goose LIBRARY AND NOT A SECOND APPLIER
//
// `goose` is pinned at v3.28.0, the version the rest of the fleet uses. Linking
// the library in at that same version means ONE migration engine describes the
// schema on a developer's machine and on the box, and one version table
// (`goose_db_version`) records where each database is. Writing a second
// applier over pgx would inherit every bug a hand-rolled runner has — the
// fleet already has the receipt for that class — so what this package owns is
// the wiring (which DSN, which files, which log, which exit status) and
// nothing about the annotations, the transaction per migration, or the
// version table.
//
// # WHAT IT DOES NOT NEED
//
// The DSN comes from `DATABASE_URL` and nothing else. A migrate Job is handed
// a database and nothing else, so requiring the service's other configuration
// to apply a migration would make the Job fail for a reason that has nothing
// to do with the schema.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/pressly/goose/v3"

	// The `database/sql` driver goose needs, and it is pgx — the Postgres
	// library this service already depends on — rather than a second
	// connection stack in the image. Imported for its side effect: it
	// registers the "pgx" driver name `sql.Open` below asks for.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// ErrNoDSN is returned when there is no database to migrate.
//
// It is a named error rather than a message so a caller can tell "the Job was
// started without a DSN" from "the DSN was refused", which are different
// conversations with different people.
var ErrNoDSN = errors.New("DATABASE_URL is not set; there is no database to migrate")

// RunTimeout bounds the whole apply. A fresh database applies press's
// migrations in seconds; the headroom is for an `ALTER TABLE` waiting on a
// lock, which is the slow case that is real. A run that is still connected
// past this gives up with an error naming the database it could not reach,
// rather than holding a deploy open saying nothing.
const RunTimeout = 2 * time.Minute

// Run applies every pending migration in fsys to the database named by dsn
// and returns the file names it applied, in order.
//
// A SECOND CALL RETURNS AN EMPTY SLICE, and that is the idempotency a
// re-run depends on: goose records every applied file in `goose_db_version`,
// so "pending" is a question about that table and not about whether the files
// look applied. A file that fails is rolled back in its own transaction, and
// the error names it.
func Run(ctx context.Context, dsn string, fsys fs.FS, logger *slog.Logger) ([]string, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, ErrNoDSN
	}
	if fsys == nil {
		return nil, errors.New("no migrations to apply: the filesystem is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening the database: %w", err)
	}
	defer func() { _ = db.Close() }()

	// sql.Open is lazy: connecting here means an unreachable database reports
	// as a connect failure with this error, rather than as a nil dereference
	// or a timeout inside goose's first query.
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connecting to the database: %w", err)
	}

	// The provider rather than the package-level goose functions: goose's
	// global API is process-wide mutable state, and this package has no reason
	// to have any. One provider, built from the call's own arguments.
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithSlog(logger))
	if err != nil {
		return nil, fmt.Errorf("reading the migrations: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		// `results` is nil on failure, deliberately: the applied set is what
		// goose's own version table already records, and the useful sentence
		// is which file stopped the run.
		return nil, nameTheFailure(err)
	}

	applied := make([]string, 0, len(results))
	for _, result := range results {
		if result.Source != nil {
			applied = append(applied, path.Base(result.Source.Path))
		}
	}
	return applied, nil
}

// nameTheFailure makes sure the error a caller prints NAMES THE FILE that
// failed, because that is the one fact a failed deploy needs and the one
// goose's own message does not carry: `PartialError.Error` reports a version
// number, and "version 1 failed" is a bisect where "00001_press.sql failed"
// is the file to open.
//
// A failure that is not a PartialError — the version table could not be read,
// the database went away mid-run — is returned unchanged: it has no file to
// name, and inventing one would be worse than saying nothing.
func nameTheFailure(err error) error {
	var partial *goose.PartialError
	if !errors.As(err, &partial) || partial.Failed == nil || partial.Failed.Source == nil {
		return err
	}
	return fmt.Errorf("%s failed: %w", path.Base(partial.Failed.Source.Path), err)
}
