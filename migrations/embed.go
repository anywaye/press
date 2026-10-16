// Package migrations carries this directory's goose SQL files into press's
// binaries, so the migrate step can apply the schema without a shell, without
// `psql` and without the goose command line tool.
//
// # WHY A GO PACKAGE LIVES IN A DIRECTORY OF SQL
//
// `go:embed` can only reach files at or below the directory of the importing
// package, so the embed has to be declared here. None of the SQL tooling sees
// the `.go` file, and none of the Go tooling sees the SQL as code: the two
// sets meet only in `internal/migrate`, which is what reads this.
//
// The pattern is `*.sql` and not the whole directory, so the set the binary
// applies is exactly the set a `*.sql` glob names.
package migrations

import "embed"

// FS is the schema, as it ships in the image.
//
//go:embed *.sql
var FS embed.FS
