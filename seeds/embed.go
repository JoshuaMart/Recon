// Package seeds carries reference data as files.
//
// It is applied by a replayable seed run on every deployment rather than by a
// migration. The lists grow as new frameworks and edges appear, and a migration
// per addition would be a migration nobody wants to write.
package seeds

import _ "embed"

// GenericPivots is the denylist of values that group without discriminating.
//
//go:embed generic_pivots.yaml
var GenericPivots []byte
