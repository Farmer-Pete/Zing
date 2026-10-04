// Package gitbin finds the git binary Zing execs, once per process.
package gitbin

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// trampoline is the path macOS's Command Line Tools install for "git":
// a stub that execs the real binary under the active developer directory,
// so every call through it costs two execs instead of one.
const trampoline = "/usr/bin/git"

var (
	once sync.Once
	path string
)

// Path returns the git binary to exec: exec.LookPath("git"), except that on
// darwin, when that is the /usr/bin/git xcrun trampoline, it is the real
// binary `xcrun --find git` names, so each call costs one exec, not two. A
// PATH shim or any other install is returned unchanged. When LookPath fails
// it returns "git", so exec fails the way it always has. The first call
// logs the outcome once through slog.Default().
func Path() string {
	once.Do(func() {
		r := resolve(runtime.GOOS, exec.LookPath, xcrunFindGit)
		path = r.path
		if r.err != nil {
			slog.Warn("git binary fallback", "path", r.path, "source", r.source, "lookpath", r.lookPath, "error", r.err.Error())
			return
		}
		slog.Info("git binary resolved", "path", r.path, "source", r.source, "lookpath", r.lookPath)
	})
	return path
}

// resolution is resolve's answer: the binary, which branch picked it, and
// the error that forced a fallback (nil when there was none).
type resolution struct {
	path     string
	source   string // "path" | "xcrun" | "xcrun_failed" | "not_found"
	lookPath string // LookPath's result, "" when it failed
	err      error
}

// resolve picks the git binary to exec. lookPath and xcrunFind are injected
// because both depend on the host; TestResolveGit drives every branch with
// fakes and real files under t.TempDir().
func resolve(goos string, lookPath func(string) (string, error), xcrunFind func() (string, error)) resolution {
	p, err := lookPath("git")
	if err != nil {
		return resolution{path: "git", source: "not_found", err: err}
	}
	if goos != "darwin" || p != trampoline {
		return resolution{path: p, source: "path", lookPath: p}
	}
	realGit, err := xcrunFind()
	if err == nil && !filepath.IsAbs(realGit) {
		err = fmt.Errorf("xcrun printed a relative path %q", realGit)
	}
	if err == nil {
		var info os.FileInfo
		if info, err = os.Stat(realGit); err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("%s is not a regular file", realGit)
		} else if err == nil && info.Mode().Perm()&0o111 == 0 {
			err = fmt.Errorf("%s is not executable", realGit)
		}
	}
	if err != nil {
		return resolution{path: p, source: "xcrun_failed", lookPath: p, err: err}
	}
	return resolution{path: realGit, source: "xcrun", lookPath: p}
}

// xcrunFindGit asks xcrun where the active developer directory's git is.
func xcrunFindGit() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/xcrun", "--find", "git").Output()
	return strings.TrimSpace(string(out)), err
}
