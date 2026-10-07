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
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"zing/internal/gitbin"
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

	// mu guards queued, hasQueued, target, carry, and hasCarry.
	mu        sync.Mutex
	queued    upgradeRequest
	hasQueued bool
	target    *restartTarget
	carry     upgradeRequest
	hasCarry  bool

	// wake has capacity 1; Request sends on it without blocking.
	wake chan struct{}

	// stop is serve's cancelServe. loop calls it at most once, when prepare
	// has set a restart target.
	stop context.CancelFunc
	// gate is unbuffered and closed exactly once, by serve right after
	// go loop in part 1 (and by part 2 only after booted_ok). loop waits
	// on it before it waits for its first wake.
	gate chan struct{}
}

// Request queues ticketID and sha as the next upgrade, newest wins. Once a
// target is set, it writes to carry instead, so the drain that already
// started is never replaced.
func (u *upgrader) Request(ticketID int64, sha string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	r := upgradeRequest{TicketID: ticketID, SHA: sha}
	if u.target != nil {
		u.carry, u.hasCarry = r, true
		return
	}
	u.queued, u.hasQueued = r, true
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

// Target reports the restart target once the loop has set one, along with
// any request that arrived after that.
func (u *upgrader) Target() (rt restartTarget, carry upgradeRequest, hasCarry, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.target == nil {
		return restartTarget{}, upgradeRequest{}, false, false
	}
	return *u.target, u.carry, u.hasCarry, true
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

// loop waits for the gate to open, then repeatedly waits for a queued
// request and runs it through runQueued, until ctx ends or runQueued sets a
// restart target and stops serve.
func (u *upgrader) loop(ctx context.Context) {
	select {
	case <-u.gate:
	case <-ctx.Done():
		return
	}
	for {
		select {
		case <-u.wake:
		case <-ctx.Done():
			return
		}
		if u.runQueued(ctx) {
			return
		}
	}
}

// runQueued pops the queued request and runs prepare on it. A build
// superseded by a newer request while it ran is discarded in favor of the
// newer one, without returning to loop's wake wait. It returns true once a
// restart target has been set, the marker is saved, and stop has been
// called; it returns false, with nothing queued, once there is nothing left
// to try.
func (u *upgrader) runQueued(ctx context.Context) bool {
	u.mu.Lock()
	if !u.hasQueued {
		u.mu.Unlock()
		return false
	}
	req := u.queued
	u.hasQueued = false
	u.mu.Unlock()

	for {
		rt, err := u.prepare(ctx, req)
		if err != nil {
			return false
		}

		if ctx.Err() != nil {
			_ = os.Remove(rt.Next)
			slog.Info("upgrade: cancelled", "from_sha", rt.FromSHA, "to_sha", rt.ToSHA, "ticket_id", rt.TicketID)
			return false
		}

		u.mu.Lock()
		if u.hasQueued {
			newer := u.queued
			u.hasQueued = false
			u.mu.Unlock()
			_ = os.Remove(rt.Next)
			slog.Info("upgrade: superseded", "from_sha", rt.FromSHA, "to_sha", newer.SHA, "ticket_id", newer.TicketID)
			req = newer
			continue
		}

		marker := upgradeMarker{FromSHA: u.running, ToSHA: rt.ToSHA, TicketID: rt.TicketID, State: markerPending}
		if err := saveUpgradeMarker(u.dataDir, marker); err != nil {
			u.mu.Unlock()
			_ = os.Remove(rt.Next)
			tellOwner(context.WithoutCancel(ctx), u.store, rt.TicketID, fmt.Sprintf("upgrade: write upgrade.json: %v", err))
			return false
		}

		u.target = &rt
		u.mu.Unlock()

		slog.Info("upgrade", "step", "drain", "from_sha", rt.FromSHA, "to_sha", rt.ToSHA, "ticket_id", rt.TicketID)
		u.stop()
		return true
	}
}

// execFunc replaces the running process image, as syscall.Exec does.
// restartAfterServe takes one as a parameter so tests can observe the call
// instead of actually replacing the test binary.
type execFunc func(argv0 string, argv, envv []string) error

// restartAfterServe renames rt.Next over rt.Binary and execs rt.Binary with
// argv and env, once ctx (run's own signal context) is still live. With a
// nil rt, or with ctx already done because a real signal arrived during the
// drain, it does nothing and returns nil, leaving zing.next and a pending
// upgrade.json for the next start to deal with.
func restartAfterServe(ctx context.Context, rt *restartTarget, argv, env []string, run execFunc) error {
	if rt == nil {
		return nil
	}
	if ctx.Err() != nil {
		// A real signal landed in the drain window, not a failure: skip the
		// swap and leave it for the next start to pick up.
		slog.Info("upgrade: restart skipped, serve was signalled", "to_sha", rt.ToSHA, "ticket_id", rt.TicketID)
		return nil //nolint:nilerr // ctx.Err() here is a signal, not a failure
	}
	if err := os.Rename(rt.Next, rt.Binary); err != nil {
		return fmt.Errorf("upgrade: swap: %w", err)
	}
	slog.Info("upgrade", "step", "exec", "from_sha", rt.FromSHA, "to_sha", rt.ToSHA, "ticket_id", rt.TicketID)
	if err := run(rt.Binary, argv, env); err != nil {
		return fmt.Errorf("upgrade: exec %s: %w", rt.Binary, err)
	}
	return nil
}

// validSHA is the only shape of merge sha Build hands to git: a full,
// lowercase commit id as the GitHub API gives it, never anything that
// could parse as a command-line option.
var validSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// gitGoSteps is the production upgradeSteps: a git fetch, a detached
// worktree at the resolved sha under tmpRoot, and a go build, then
// zing selftest and zing version on the result.
type gitGoSteps struct {
	repoGit       string
	defaultBranch string
	tmpRoot       string
}

// tailRedacted is the last at most 4096 bytes of b, with redactURLs
// applied, for use in an error that may reach a log or a ticket.
func tailRedacted(b []byte) string {
	if len(b) > 4096 {
		b = b[len(b)-4096:]
	}
	return redactURLs(string(b))
}

// runGit runs one git command against g.repoGit with GIT_TERMINAL_PROMPT=0
// added, and returns its combined output.
func (g gitGoSteps) runGit(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"--git-dir", g.repoGit}, args...)
	cmd := exec.CommandContext(ctx, gitbin.Path(), full...) //nolint:gosec // G204: args are fixed strings and a sha already checked against validSHA
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd.CombinedOutput()
}

// Build checks sha's shape before anything else, then fetches, resolves it
// (or origin/defaultBranch when sha is empty), builds it in a detached
// worktree, and returns the resolved full sha.
func (g gitGoSteps) Build(ctx context.Context, sha, out string) (string, error) {
	if sha != "" && !validSHA.MatchString(sha) {
		return "", fmt.Errorf("build: invalid sha %s", sha)
	}

	if o, err := g.runGit(ctx, "fetch", "origin", g.defaultBranch); err != nil {
		return "", fmt.Errorf("build: fetch: %w: %s", err, tailRedacted(o))
	}

	rev := sha + "^{commit}"
	if sha == "" {
		rev = "origin/" + g.defaultBranch + "^{commit}"
	}
	o, err := g.runGit(ctx, "rev-parse", "--verify", "--end-of-options", rev)
	if err != nil {
		return "", fmt.Errorf("build: rev-parse: %w: %s", err, tailRedacted(o))
	}
	resolved := strings.TrimSpace(string(o))
	if !validSHA.MatchString(resolved) {
		return "", fmt.Errorf("build: invalid sha %s", resolved)
	}

	o, err = g.runGit(ctx, "worktree", "prune")
	if err != nil {
		return "", fmt.Errorf("build: worktree prune: %w: %s", err, tailRedacted(o))
	}

	if err = os.MkdirAll(g.tmpRoot, 0o700); err != nil {
		return "", fmt.Errorf("build: mkdir %s: %w", g.tmpRoot, err)
	}
	shortSHA := resolved[:12]
	wt := filepath.Join(g.tmpRoot, "upgrade-"+shortSHA)
	o, err = g.runGit(ctx, "worktree", "add", "--detach", wt, resolved)
	if err != nil {
		return "", fmt.Errorf("build: worktree add: %w: %s", err, tailRedacted(o))
	}
	defer func() {
		if removeOut, removeErr := g.runGit(context.WithoutCancel(ctx), "worktree", "remove", "--force", wt); removeErr != nil {
			slog.Warn("upgrade: worktree remove", "error", removeErr, "output", tailRedacted(removeOut))
		}
	}()

	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("build: go: %w", err)
	}
	build := exec.CommandContext(ctx, goBin, "build", "-o", out, "./cmd/zing") //nolint:gosec // G204: goBin from LookPath, out and args fixed
	build.Dir = wt
	build.Env = os.Environ()
	if o, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build: go build: %w: %s", err, tailRedacted(o))
	}

	return resolved, nil
}

// Selftest runs bin selftest, then bin version, and trims the "zing "
// prefix from the version.
func (g gitGoSteps) Selftest(ctx context.Context, bin string) (version, output string, err error) {
	selftestOut, err := exec.CommandContext(ctx, bin, "selftest").CombinedOutput() //nolint:gosec // G204: bin is the freshly built candidate, not caller input
	if err != nil {
		return "", "", fmt.Errorf("selftest: %w: %s", err, tailRedacted(selftestOut))
	}
	versionOut, err := exec.CommandContext(ctx, bin, "version").CombinedOutput() //nolint:gosec // G204: bin is the freshly built candidate, not caller input
	if err != nil {
		return "", "", fmt.Errorf("selftest: version: %w: %s", err, tailRedacted(versionOut))
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(versionOut)), "zing ")
	return v, string(selftestOut), nil
}
