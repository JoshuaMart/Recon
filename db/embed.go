// Package db embeds the SQL migrations into the binary.
package db

import "embed"

// Migrations holds the goose migrations, applied at startup.
//
//go:embed migrations/*.sql
var Migrations embed.FS
