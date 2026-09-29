//go:build !darwin

package sandbox

import "errors"

// darwinUserCacheDir has no meaning off macOS. Load's own runtime.GOOS check
// always returns before this could be called in practice; it exists so this
// package builds on every platform CI targets (Linux included).
func darwinUserCacheDir() (string, error) {
	return "", errors.New("sandbox: darwin user cache dir is only available on macOS")
}
