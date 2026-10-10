// Package db carries the migrations as data, embedded so a binary is the whole
// deliverable (a container hunting for a directory beside itself fails at the worst
// moment). sqlc reads this same directory, not a parallel copy.
package db

import "embed"

// Migrations holds the versioned SQL, numbered sequentially rather than by
// timestamp so directory order is apply order.
//
//go:embed migrations/*.sql
var Migrations embed.FS
