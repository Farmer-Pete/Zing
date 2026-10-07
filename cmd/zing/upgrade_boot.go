// upgrade_boot.go holds selfUpgrade, the hand-off between run and serve
// (#109 part 1): run resolves the running executable and version once,
// before zing.toml is even read, and serve later fills in su.next once an
// upgrade's restart target is ready.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync/atomic"
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

// removeMarker removes DATA_DIR/upgrade.json; a missing file is fine.
func removeMarker(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, upgradeMarkerFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("upgrade: remove upgrade.json: %w", err)
	}
	return nil
}

// rollbackTarget is the exec a rollback performs: rollBack has already
// renamed zing.prev over exe, so Next is empty and restartAfterServe does
// no swap of its own.
func rollbackTarget(m upgradeMarker, exe string) restartTarget {
	return restartTarget{Binary: exe, FromSHA: m.ToSHA, ToSHA: m.FromSHA, TicketID: m.TicketID}
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

	// boot is set by bootAndServe from guardBoot's answer, before serve
	// runs. It is never bootRollback, since bootAndServe execs instead of
	// serving in that case.
	boot bootAction
	// marker is the upgrade.json guardBoot read, with State already
	// updated to attempted for a watch boot; it is the zero value for
	// bootNormal.
	marker upgradeMarker
	// booted is set once, by onBooted, once the boot watch sees a 200. The
	// 60 s timer and finishBoot both read it.
	booted atomic.Bool
	// deadlinePassed is set once, by the 60 s timer, when it fires before
	// booted is set. finishBoot reads it to pick failed's cause.
	deadlinePassed atomic.Bool
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

// guardBoot reads upgrade.json under serve.lock, before zing.toml is read,
// and acts on decideBoot's answer. A watch marks the marker attempted; a
// rollback renames zing.prev over exe and marks it rolled_back; a discard
// removes it. It releases serve.lock before it returns. Any error comes
// back with the action guardBoot settled on, for bootAndServe to log at
// WARN. With a missing dataDir or an empty exe, it returns normal without
// touching anything.
func guardBoot(dataDir, exe, running string) (bootAction, upgradeMarker, restartTarget, error) {
	if exe == "" {
		return bootNormal, upgradeMarker{}, restartTarget{}, nil
	}
	resolved, err := filepath.EvalSymlinks(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return bootNormal, upgradeMarker{}, restartTarget{}, nil
	}
	if err != nil {
		return bootNormal, upgradeMarker{}, restartTarget{}, fmt.Errorf("upgrade: boot guard: resolve %s: %w", dataDir, err)
	}
	binary := filepath.Join(resolved, "bin", "zing")
	if exe != binary {
		slog.Warn(fmt.Sprintf("upgrade: boot guard skipped: serve runs %s, not %s", exe, binary))
		return bootNormal, upgradeMarker{}, restartTarget{}, nil
	}

	lock, err := acquireServeLock(resolved)
	if err != nil {
		return bootNormal, upgradeMarker{}, restartTarget{}, err
	}
	defer lock.release()

	m, found, err := loadUpgradeMarker(resolved)
	if err != nil {
		return bootNormal, upgradeMarker{}, restartTarget{}, err
	}
	switch action := decideBoot(m, found, running); action {
	case bootWatch:
		m.State = markerAttempted
		slog.Info("upgrade", "step", "boot_watch", "from_sha", m.FromSHA, "to_sha", m.ToSHA, "ticket_id", m.TicketID)
		return bootWatch, m, restartTarget{}, saveUpgradeMarker(resolved, m)
	case bootRollback:
		return rollBack(resolved, exe, m)
	case bootDiscard:
		slog.Warn("upgrade: boot guard discarded upgrade.json", "state", m.State, "to_sha", m.ToSHA, "running", running, "ticket_id", m.TicketID)
		return bootDiscard, m, restartTarget{}, removeMarker(resolved)
	default: // bootNormal, bootReport
		return action, m, restartTarget{}, nil
	}
}

// rollBack renames zing.prev over exe and marks the marker rolled_back.
// With no zing.prev there is nothing to roll back to: it removes the
// marker and answers normal.
func rollBack(dataDir, exe string, m upgradeMarker) (bootAction, upgradeMarker, restartTarget, error) {
	prev := exe + ".prev"
	if err := os.Rename(prev, exe); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("upgrade: rollback: no zing.prev", "to_sha", m.ToSHA, "ticket_id", m.TicketID)
			return bootNormal, m, restartTarget{}, removeMarker(dataDir)
		}
		return bootNormal, m, restartTarget{}, fmt.Errorf("upgrade: rollback: %w", err)
	}
	m.State = markerRolledBack
	slog.Warn("upgrade: rolling back", "from_sha", m.ToSHA, "to_sha", m.FromSHA, "ticket_id", m.TicketID)
	return bootRollback, m, rollbackTarget(m, exe), saveUpgradeMarker(dataDir, m)
}
