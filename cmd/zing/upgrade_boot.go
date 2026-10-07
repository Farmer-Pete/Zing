// upgrade_boot.go holds selfUpgrade, the hand-off between run and serve
// (#109 part 1): run resolves the running executable and version once,
// before zing.toml is even read, and serve later fills in su.next once an
// upgrade's restart target is ready.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
)

// bootAction is what the boot guard decides to do with upgrade.json at
// start-up, before zing.toml is read.
type bootAction int

const (
	bootNormal bootAction = iota
	bootWatch
	bootRollback
	bootReport
	bootDiscard
)

// String renders a bootAction as a word, for log fields.
func (a bootAction) String() string {
	switch a {
	case bootNormal:
		return "normal"
	case bootWatch:
		return "watch"
	case bootRollback:
		return "rollback"
	case bootReport:
		return "report"
	case bootDiscard:
		return "discard"
	default:
		return fmt.Sprintf("boot(%d)", int(a))
	}
}

// bootOutcome's results, and the causes a failed outcome names.
const (
	outcomeNone   = "none"
	outcomeFailed = "failed"
	outcomeRevert = "revert"

	bootCauseDeadline = "deadline"
	bootCauseStopped  = "serve_stopped"
)

// decideBoot maps upgrade.json, as found at start-up, to what the boot
// guard does with it.
func decideBoot(m upgradeMarker, found bool, running string) bootAction {
	if !found {
		return bootNormal
	}
	matches := matchesRunning(m.ToSHA, running)
	switch m.State {
	case markerPending:
		if matches {
			return bootWatch
		}
		return bootDiscard
	case markerAttempted:
		if matches {
			return bootRollback
		}
		return bootDiscard
	case markerRolledBack:
		return bootReport
	default:
		return bootDiscard
	}
}

// bootOutcome decides how a serve that has returned ends its boot. cause
// names why a failed boot failed, for finishBoot's log; it is empty for
// none and revert.
func bootOutcome(watching, booted, deadlinePassed, signalled bool) (outcome, cause string) {
	switch {
	case !watching || booted:
		return outcomeNone, ""
	case signalled:
		return outcomeRevert, ""
	case deadlinePassed:
		return outcomeFailed, bootCauseDeadline
	default:
		return outcomeFailed, bootCauseStopped
	}
}

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
	path, err := os.Executable()
	if err != nil {
		slog.Warn("upgrade: resolve executable", "error", err)
	} else if resolved, evalErr := filepath.EvalSymlinks(path); evalErr != nil {
		slog.Warn("upgrade: resolve executable", "error", evalErr)
	} else {
		exe = resolved
	}
	info, _ := debug.ReadBuildInfo()
	return &selfUpgrade{exe: exe, running: versionString(info)}
}
