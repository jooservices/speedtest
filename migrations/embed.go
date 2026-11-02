// Package migrations contains the versioned PostgreSQL schema embedded in binaries.
package migrations

import "embed"

// Files contains immutable, ordered goose SQL migrations.
//
//go:embed *.sql
var Files embed.FS
