// Package db carries the migrations as data.
//
// They are embedded rather than read from disk so that a binary is the whole
// deliverable: a container that has to find a directory next to itself is a
// container that fails at the worst moment, and the schema sqlc reads later is
// this same directory rather than a parallel copy of it.
package db

import "embed"

// Migrations holds the versioned SQL, numbered sequentially rather than by
// timestamp so that the order in a directory listing is the order they apply.
//
//go:embed migrations/*.sql
var Migrations embed.FS
