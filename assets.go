// Package zing holds the embedded process definition: machine.toml plus
// every prompt, style, and lens file under prompts/. go:embed cannot reach
// parent directories, so this file lives at the repo root, next to them.
package zing

import "embed"

//go:embed machine.toml prompts
var Assets embed.FS
