// upgrade_boot.go holds selfUpgrade, the hand-off between run and serve
// (#109 part 1): run resolves the running executable and version once,
// before zing.toml is even read, and serve later fills in su.next once an
// upgrade's restart target is ready.
package main

import (
	"os"
	"path/filepath"
	"runtime/debug"
)

// selfUpgrade carries the self-upgrade state across serve's call, from run
// to the restartAfterServe call that follows it.
type selfUpgrade struct {
	// exe is the running executable, symlinks resolved, or empty when it
	// could not be resolved. newUpgrader copies it into upgrader.exe, which
	// prepare's install_check then refuses to match as DATA_DIR/bin/zing.
	exe string
	// running is this build's own version string (versionString), copied
	// into upgrader.running for matchesRunning.
	running string
	// next is set by serve, after shutdown, when Target reports a restart
	// target is ready. run passes it to restartAfterServe.
	next *restartTarget
}

// newSelfUpgrade resolves the running executable and this build's version,
// for newUpgrader. A resolve failure leaves exe empty.
func newSelfUpgrade() *selfUpgrade {
	exe := ""
	if path, err := os.Executable(); err == nil {
		if resolved, evalErr := filepath.EvalSymlinks(path); evalErr == nil {
			exe = resolved
		}
	}
	info, _ := debug.ReadBuildInfo()
	return &selfUpgrade{exe: exe, running: versionString(info)}
}
