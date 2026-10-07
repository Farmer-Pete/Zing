// upgrade.go implements the self-upgrade upgrader: building a merged self
// pull request, testing it, backing up the database, and handing off a
// restart target after serve drains.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"zing/internal/store"
)

// urlUserinfo matches the userinfo component of a URL, so redactURLs can
// strip it before a message reaches a log or a ticket.
var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)

// redactURLs turns scheme://USERINFO@ into scheme://REDACTED@.
func redactURLs(s string) string { return urlUserinfo.ReplaceAllString(s, "${1}REDACTED@") }

// tellOwner logs text at WARN, which the alerts view shows, and posts it on
// ticketID as a system update message when ticketID is above 0.
func tellOwner(ctx context.Context, st *store.Store, ticketID int64, text string) {
	text = redactURLs(text)
	slog.Warn("upgrade", "ticket_id", ticketID, "error", text)
	if ticketID <= 0 {
		return
	}
	if _, err := st.InsertMessage(ctx, store.Message{TicketID: ticketID, Type: "update", Author: "system", Body: text}); err != nil {
		slog.Warn("upgrade: post message", "ticket_id", ticketID, "error", err)
	}
}

// errUpgradeNotRun marks a prepare outcome that needed no owner message,
// either because install_check refused to run or because the built sha is
// already what's running.
var errUpgradeNotRun = errors.New("upgrade: not run")

const (
	upgradeBuildTimeout    = 10 * time.Minute
	upgradeSelftestTimeout = 5 * time.Minute
)

// upgradeRequest names one merge to upgrade to.
type upgradeRequest struct {
	TicketID int64
	SHA      string
}

// restartTarget is the swap-and-exec the owning serve performs after it
// drains: rename Next over Binary, then exec Binary.
type restartTarget struct {
	Binary   string
	Next     string
	FromSHA  string
	ToSHA    string
	TicketID int64
}

// upgradeSteps builds and verifies one candidate binary. gitGoSteps is the
// production implementation; tests use a fake.
type upgradeSteps interface {
	// Build writes a binary for sha to out and returns the resolved full sha.
	Build(ctx context.Context, sha, out string) (string, error)
	// Selftest runs bin selftest, then bin version. version has the "zing "
	// prefix trimmed.
	Selftest(ctx context.Context, bin string) (version, output string, err error)
}

// upgrader drives one project's self-upgrade: building a merged commit,
// verifying it, and handing off a restart target.
type upgrader struct {
	dataDir string
	exe     string
	running string
	store   *store.Store
	steps   upgradeSteps
}

// matchesRunning reports whether running names commit sha: running must be
// stamped (not versionFallback, not ending in versionDirtySuffix), at least
// 7 characters, and a prefix of sha.
func matchesRunning(sha, running string) bool {
	stamped := running != versionFallback && !strings.HasSuffix(running, versionDirtySuffix)
	return stamped && len(running) >= 7 && strings.HasPrefix(sha, running)
}

// sha12 is the first 12 bytes of sha, or all of it when shorter, for use in
// log fields and owner-facing messages.
func sha12(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// prepare builds req.SHA, verifies it, backs up the database, and keeps the
// current binary as zing.prev. Every failure removes zing.next and, unless
// ctx is already done, calls tellOwner.
func (u *upgrader) prepare(ctx context.Context, req upgradeRequest) (restartTarget, error) {
	binary := filepath.Join(u.dataDir, "bin", "zing")
	next := binary + ".next"
	prev := binary + ".prev"

	logStep := func(step, toSHA string) {
		slog.Info("upgrade", "step", step, "from_sha", u.running, "to_sha", toSHA, "ticket_id", req.TicketID)
	}
	fail := func(err error) (restartTarget, error) {
		_ = os.Remove(next)
		if ctx.Err() != nil {
			slog.Info("upgrade: cancelled", "from_sha", u.running, "to_sha", req.SHA, "ticket_id", req.TicketID)
		} else {
			tellOwner(context.WithoutCancel(ctx), u.store, req.TicketID, err.Error())
		}
		return restartTarget{}, err
	}

	logStep("install_check", req.SHA)
	if u.exe != binary {
		msg := fmt.Sprintf("upgrade: not run: serve runs %s, not %s", u.exe, binary)
		tellOwner(ctx, u.store, req.TicketID, msg)
		return restartTarget{}, errUpgradeNotRun
	}

	logStep("build", req.SHA)
	buildCtx, cancelBuild := context.WithTimeout(ctx, upgradeBuildTimeout)
	builtSHA, err := u.steps.Build(buildCtx, req.SHA, next)
	cancelBuild()
	if err != nil {
		return fail(fmt.Errorf("upgrade: build %s: %w", sha12(req.SHA), err))
	}

	if matchesRunning(builtSHA, u.running) {
		_ = os.Remove(next)
		slog.Info("upgrade: already running", "from_sha", u.running, "to_sha", builtSHA, "ticket_id", req.TicketID)
		return restartTarget{}, errUpgradeNotRun
	}

	logStep("selftest", builtSHA)
	selftestCtx, cancelSelftest := context.WithTimeout(ctx, upgradeSelftestTimeout)
	version, _, err := u.steps.Selftest(selftestCtx, next)
	cancelSelftest()
	if err != nil {
		return fail(fmt.Errorf("upgrade: selftest of %s failed: %w", sha12(builtSHA), err))
	}

	logStep("version_check", builtSHA)
	if !matchesRunning(builtSHA, version) {
		return fail(fmt.Errorf("upgrade: zing.next reports version %s, not %s", version, builtSHA))
	}

	logStep("backup", builtSHA)
	backupPath := filepath.Join(u.dataDir, backupPrefix+sha12(builtSHA))
	if err := u.store.BackupTo(ctx, backupPath); err != nil {
		return fail(fmt.Errorf("upgrade: backup: %w", err))
	}
	if err := pruneBackups(u.dataDir, backupKeep); err != nil {
		slog.Warn("upgrade: prune backups", "error", err)
	}

	logStep("keep_prev", builtSHA)
	if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(fmt.Errorf("upgrade: keep zing.prev: %w", err))
	}
	if err := os.Link(binary, prev); err != nil {
		return fail(fmt.Errorf("upgrade: keep zing.prev: %w", err))
	}

	return restartTarget{Binary: binary, Next: next, FromSHA: u.running, ToSHA: builtSHA, TicketID: req.TicketID}, nil
}
