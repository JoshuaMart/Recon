// Package seeds carries reference data as files, applied by a replayable seed on
// every deployment rather than a migration: the lists grow with new frameworks and
// edges, and a migration per addition is one nobody wants to write.
package seeds

import _ "embed"

// GenericPivots is the denylist of values that group without discriminating.
//
//go:embed generic_pivots.yaml
var GenericPivots []byte
