// Package fixtures embeds the repo-owned fixtures: the tracker's intake
// tickets and the fake runtime's scripts.
package fixtures

import "embed"

// FS is the embedded fixtures tree. The tracker fixture reads tickets.toml
// from it; the fake runtime reads its scripts from the scripts/ subtree
// (fs.Sub(FS, "scripts")).
//
//go:embed tickets.toml scripts
var FS embed.FS
