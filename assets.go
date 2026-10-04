// Package knotra provides deployment assets embedded into the CLI distribution.
package knotra

import _ "embed"

// Compose is the development infrastructure used by quickstart and repository tests.
//
//go:embed compose.yaml
var Compose []byte
