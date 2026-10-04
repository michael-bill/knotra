// Package starter contains the same example packages distributed in the source
// repository. Embedding keeps the first-run command independent of a checkout.
package starter

import "embed"

// Files contains editable pipeline packages, without test or documentation files.
//
//go:embed hello research-dossier tic-tac-toe publication
var Files embed.FS
