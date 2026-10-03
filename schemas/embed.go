// Package schemas contains the versioned, embedded structural language schemas.
package schemas

import _ "embed"

// V1 is the authoritative structural schema for Pipeline and EngineProfile.
//
//go:embed knotra-v1.schema.json
var V1 []byte
