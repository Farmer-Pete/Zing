//go:build darwin

package sandbox

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// darwinUserCacheDirTimeout bounds the getconf call below: a fixed,
// external command that should return instantly, but Load must never hang
// on it.
const darwinUserCacheDirTimeout = 5 * time.Second

// darwinUserCacheDir runs `getconf DARWIN_USER_CACHE_DIR` and returns its
// trimmed output (section 5.2): the per-user cache folder whose "mds" child
// is MDS_CACHE.
func darwinUserCacheDir() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), darwinUserCacheDirTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "getconf", "DARWIN_USER_CACHE_DIR").Output() //nolint:gosec // G204: fixed argv, no external input
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
