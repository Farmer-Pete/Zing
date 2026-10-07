// upgrade_boot.go holds selfUpgrade, the hand-off between run and serve
// (#109 part 1): run resolves the running executable and version once,
// before zing.toml is even read, and serve later fills in su.next once an
// upgrade's restart target is ready.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync/atomic"
	"time"

	"zing/internal/store"
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

	// closeBootedOK is closeUpgrade's outcome for a watch boot that answered
	// 200; the rolled_back outcome reuses markerRolledBack.
	closeBootedOK = "booted_ok"
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

// watchBoot GETs url at once and then every interval until it answers 200
// (true) or ctx ends (false). Each GET has its own 2 s timeout. Each poll
// that is not a 200 logs at DEBUG; giving up logs one WARN with the last
// poll's status and error, so a failed boot can be diagnosed after the
// rollback.
func watchBoot(ctx context.Context, url string, every time.Duration) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	var lastStatus, polls int
	var lastErr error
	for {
		status, err := bootAnswered(ctx, client, url)
		if ctx.Err() == nil {
			polls++
			if err == nil && status == http.StatusOK {
				return true
			}
			lastStatus, lastErr = status, err
			slog.Debug("upgrade: boot watch poll", "url", url, "status", status, "error", err)
		}
		select {
		case <-ctx.Done():
			slog.Warn("upgrade: boot watch gave up", "url", url, "last_status", lastStatus, "last_error", lastErr, "polls", polls)
			return false
		case <-ticker.C:
		}
	}
}

// bootAnswered GETs url once and returns its status code, or 0 and the
// error when the request could not be built or sent.
func bootAnswered(ctx context.Context, client *http.Client, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

// closeUpgrade records how an upgrade ended and removes upgrade.json.
// request is up.Request, or nil with no upgrader; it receives a carried
// request on booted_ok. Both outcomes post a message only when
// m.TicketID is above 0.
func closeUpgrade(ctx context.Context, st *store.Store, dataDir string, m upgradeMarker, outcome string, request func(int64, string)) {
	switch outcome {
	case closeBootedOK:
		slog.Info("upgrade", "step", closeBootedOK, "from_sha", m.FromSHA, "to_sha", m.ToSHA, "ticket_id", m.TicketID)
		if m.TicketID > 0 {
			body := closeBootedOK + " " + sha12(m.ToSHA)
			if _, err := st.InsertMessage(ctx, store.Message{TicketID: m.TicketID, Type: "update", Author: "system", Body: body}); err != nil {
				slog.Warn("upgrade: post message", "ticket_id", m.TicketID, "error", err)
			}
		}
	case markerRolledBack:
		tellOwner(ctx, st, m.TicketID, fmt.Sprintf("upgrade: %s rolled back to %s: it did not answer 200 within 60 s", sha12(m.ToSHA), sha12(m.FromSHA)))
	default:
		slog.Warn("upgrade: close: unknown outcome", "outcome", outcome, "ticket_id", m.TicketID)
		return
	}
	if err := removeMarker(dataDir); err != nil {
		slog.Warn("upgrade: close", "outcome", outcome, "ticket_id", m.TicketID, "error", err)
	}
	if outcome == closeBootedOK && m.HasNext && request != nil {
		slog.Info("upgrade: carried request passed on", "ticket_id", m.NextTicketID, "sha", m.NextSHA)
		request(m.NextTicketID, m.NextSHA)
	}
}
