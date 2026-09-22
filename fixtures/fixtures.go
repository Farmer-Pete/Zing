// Package fixtures embeds the repo-owned fixtures: the tracker's intake
// tickets and, in a later task, the fake runtime's scripts.
package fixtures

import "embed"

// FS is the embedded fixtures tree. The tracker fixture reads tickets.toml
// from it; a later task widens this embed to include scripts/.
//
//go:embed tickets.toml
var FS embed.FS
