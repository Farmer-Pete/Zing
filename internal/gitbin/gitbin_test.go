package gitbin

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

const (
	darwinGOOS = "darwin"
	linuxGOOS  = "linux"
	shimGit    = "/tmp/shim/git"

	sourcePath        = "path"
	sourceXcrun       = "xcrun"
	sourceXcrunFailed = "xcrun_failed"
	sourceNotFound    = "not_found"
)

// TestResolveGit proves resolve's branching: LookPath failure, a non-darwin
// or non-trampoline LookPath result returned unchanged, and every outcome
// of asking xcrun for the real binary behind the darwin /usr/bin/git
// trampoline.
func TestResolveGit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	realGit := filepath.Join(dir, "real-git")
	if err := os.WriteFile(realGit, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // G306: test fixture, not production output
		t.Fatalf("write realGit: %v", err)
	}
	dirGit := filepath.Join(dir, "dir-git")
	if err := os.Mkdir(dirGit, 0o755); err != nil {
		t.Fatalf("mkdir dirGit: %v", err)
	}
	missingGit := filepath.Join(dir, "missing-git")

	lookPathErr := errors.New("exec: \"git\": executable file not found in $PATH")
	xcrunErr := errors.New("xcrun: error: unable to find utility \"git\"")

	unexpectedXcrun := func(t *testing.T) func() (string, error) {
		t.Helper()
		return func() (string, error) {
			t.Fatal("xcrunFind called unexpectedly")
			return "", nil
		}
	}

	cases := []struct {
		name      string
		goos      string
		lookPath  func(string) (string, error)
		xcrunFind func() (string, error)
		want      resolution
	}{
		{
			name:     "lookpath failure",
			goos:     darwinGOOS,
			lookPath: func(string) (string, error) { return "", lookPathErr },
			want:     resolution{path: "git", source: sourceNotFound, err: lookPathErr},
		},
		{
			name:     "linux usr bin git is not the trampoline",
			goos:     linuxGOOS,
			lookPath: func(string) (string, error) { return trampoline, nil },
			want:     resolution{path: trampoline, source: sourcePath, lookPath: trampoline},
		},
		{
			name:     "darwin shim is returned unchanged",
			goos:     darwinGOOS,
			lookPath: func(string) (string, error) { return shimGit, nil },
			want:     resolution{path: shimGit, source: sourcePath, lookPath: shimGit},
		},
		{
			name:     "darwin trampoline resolved by xcrun",
			goos:     darwinGOOS,
			lookPath: func(string) (string, error) { return trampoline, nil },
			xcrunFind: func() (string, error) {
				return realGit, nil
			},
			want: resolution{path: realGit, source: sourceXcrun, lookPath: trampoline},
		},
		{
			name:     "xcrun itself fails",
			goos:     darwinGOOS,
			lookPath: func(string) (string, error) { return trampoline, nil },
			xcrunFind: func() (string, error) {
				return "", xcrunErr
			},
			want: resolution{path: trampoline, source: sourceXcrunFailed, lookPath: trampoline, err: xcrunErr},
		},
		{
			name:     "xcrun prints a relative path",
			goos:     darwinGOOS,
			lookPath: func(string) (string, error) { return trampoline, nil },
			xcrunFind: func() (string, error) {
				return "git", nil
			},
			want: resolution{path: trampoline, source: sourceXcrunFailed, lookPath: trampoline, err: errors.New("relative")},
		},
		{
			name:     "xcrun names a missing path",
			goos:     darwinGOOS,
			lookPath: func(string) (string, error) { return trampoline, nil },
			xcrunFind: func() (string, error) {
				return missingGit, nil
			},
			want: resolution{path: trampoline, source: sourceXcrunFailed, lookPath: trampoline, err: errors.New("missing")},
		},
		{
			name:     "xcrun names a directory",
			goos:     darwinGOOS,
			lookPath: func(string) (string, error) { return trampoline, nil },
			xcrunFind: func() (string, error) {
				return dirGit, nil
			},
			want: resolution{path: trampoline, source: sourceXcrunFailed, lookPath: trampoline, err: errors.New("not a regular file")},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			xcrunFind := c.xcrunFind
			if xcrunFind == nil {
				xcrunFind = unexpectedXcrun(t)
			}

			got := resolve(c.goos, c.lookPath, xcrunFind)

			if got.path != c.want.path {
				t.Errorf("path = %q, want %q", got.path, c.want.path)
			}
			if got.source != c.want.source {
				t.Errorf("source = %q, want %q", got.source, c.want.source)
			}
			if got.lookPath != c.want.lookPath {
				t.Errorf("lookPath = %q, want %q", got.lookPath, c.want.lookPath)
			}
			if (got.err == nil) != (c.want.err == nil) {
				t.Errorf("err = %v, want err == nil: %v", got.err, c.want.err == nil)
			}
		})
	}
}

// TestPathSkipsXcrunTrampoline proves Path() on a real Mac resolves past
// /usr/bin/git's xcrun trampoline to a real, executable binary.
func TestPathSkipsXcrunTrampoline(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "darwin" {
		t.Skip("darwin only")
	}
	lookPath, err := exec.LookPath("git")
	if err != nil || lookPath != trampoline {
		t.Skipf("exec.LookPath(%q) = %q, %v; want %q, nil", "git", lookPath, err, trampoline)
	}

	got := Path()
	if !filepath.IsAbs(got) {
		t.Fatalf("Path() = %q, want an absolute path", got)
	}
	if got == trampoline {
		t.Fatalf("Path() = %q, want the real binary behind the trampoline", got)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat Path() = %q: %v", got, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("Path() = %q is not a regular file", got)
	}

	out, err := exec.CommandContext(t.Context(), got, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v: %s", got, err, out)
	}
}
