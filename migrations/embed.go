package migrations

import "embed"

// FS is the schema, as it ships in the image.
//
//go:embed *.sql
var FS embed.FS
