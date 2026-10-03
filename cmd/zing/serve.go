package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	zing "zing"
	"zing/internal/bus"
	"zing/internal/config"
	"zing/internal/console"
	zdispatch "zing/internal/dispatch"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/notify"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
	"zing/internal/tracker"
)

// The model alias names config.Models's fields key (matching machine.toml's
// job.model values) and the runtime names machine.toml's job.runtime field
// allows, named once here so goconst has nothing to flag across this file
// and selftest.go, both package main.
const (
	modelAliasSonnet = "sonnet"
	modelAliasOpus   = "opus"
	modelAliasFable  = "fable"
	modelAliasCodex  = "codex"

	runtimeNameClaude = "claude"
	runtimeNameCodex  = "codex"
	runtimeNameFake   = "fake"
)

// drainDeadline bounds how long serve waits for the dispatcher's Run
// goroutine to finish its current tick before force-cancelling it (design
// section 6.10). A tick that is still running past this deadline gets its
// context cancelled so the store is never closed under a live handler.
const drainDeadline = 30 * time.Second

// shutdownTimeout bounds srv.Shutdown, which by then only has to close idle
// connections and let SSE handlers unwind, since RegisterOnShutdown already
// cancelled their base context.
const shutdownTimeout = 10 * time.Second

// defaultDispatchInterval and defaultDispatchMaxParallel are the documented
// defaults for cfg.Dispatch.IntervalSeconds and cfg.Dispatch.MaxParallel
// (internal/config's applyDefaults uses the same two values). serve clamps
// to these here, rather than in internal/config, because applyDefaults only
// fires when a key is absent from zing.toml: an explicit non-positive value
// (interval_seconds = 0, max_parallel = -1) survives config.Load untouched
// and would otherwise panic time.Ticker (interval) or stall all processing
// (max_parallel, since Tick never claims once active >= max_parallel).
const (
	defaultDispatchInterval    = 30 * time.Second
	defaultDispatchMaxParallel = 2
)

// parseServeFlags parses the "serve" subcommand's own flags (args is
// everything after "serve", os.Args[2:] shaped, matching runValidate's own
// args[2:] convention). --seed-demo (design section 6.15, section 12 row
// 12) is off by default: the normal serve path never seeds, and only a
// caller that passes the flag explicitly gets SeedDemo run once at startup,
// for hands-on verification.
func parseServeFlags(args []string) (seedDemo bool, err error) {
	flagSet := flag.NewFlagSet("serve", flag.ContinueOnError)
	flagSet.BoolVar(&seedDemo, "seed-demo", false, "seed one demo project and ticket (design section 6.15) once at startup; never on by default")
	if err := flagSet.Parse(args); err != nil {
		return false, fmt.Errorf("parse serve flags: %w", err)
	}
	return seedDemo, nil
}

// run wires the default paths and the signal-derived base context, then
// hands off to serve. It is the "serve" subcommand's entry point; args is
// os.Args[2:], the arguments after "serve".
func run(args []string) error {
	seedDemo, err := parseServeFlags(args)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfgPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	dbPath, err := store.DefaultPath()
	if err != nil {
		return err
	}

	return serve(ctx, cfgPath, dbPath, seedDemo)
}

// serve starts the store, the dispatcher, and the console, and runs until
// ctx is cancelled (or the console listener fails), draining the dispatcher
// before it closes the store (design section 6.10). seedDemo, true only
// when the "serve" subcommand was given --seed-demo, calls
// console.SeedDemo once, right after the store opens and its control flags
// are cleared, so the demo project and ticket (design section 6.15) are
// visible before the console's first request; false leaves the store
// exactly as a normal serve always has, since SeedDemo must never run
// unasked.
func serve(ctx context.Context, cfgPath, dbPath string, seedDemo bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	// Resolved before store.Open, so a console.bind that resolves to no
	// address at all fails before any database file is created (design
	// section 6.14; matches the single-bind skeleton's own
	// consoleBindAddr, which this replaces). A literal IP passes straight
	// through; "tailscale" resolves CLI-first, falling back to an
	// interface scan, and is skipped -- not an error -- when it cannot
	// resolve (bind.go).
	hosts := resolveBindHosts(ctx, cfg.Console.Bind, runTailscaleCLI, realTailscaleInterfaces)
	if len(hosts) == 0 {
		return fmt.Errorf("zing.toml: console.bind: no address could be resolved from %v", cfg.Console.Bind)
	}

	// One serve per data directory (design D7, section 6.2): acquired
	// right here, before store.Open, so a second serve against the same
	// data directory never opens the store or runs the tmp/judge sweeps
	// below. dataDir here is unresolved (filepath.Dir(dbPath) as given,
	// not EvalSymlinks'd): the lock files live next to zing.db itself,
	// wherever that path actually points.
	lock, err := acquireServeLock(filepath.Dir(dbPath))
	if err != nil {
		return err
	}
	defer lock.release()

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}

	// Built before the log handler (design section 6a), so its Publish
	// method can be wired in as onWarn: a fresh WARN-or-above record then
	// wakes every open console stream, the same wake-up path a store change
	// already uses, so the alerts view (internal/console/log.go's Warnings,
	// patchRegions' #alerts patch) needs no signal of its own.
	b := bus.New()

	logHandler, err := installLogHandler(ctx, st, b)
	if err != nil {
		_ = st.Close()
		return err
	}

	// Clear the persisted control flags a prior graceful stop may have left
	// set. Without this, the "draining" flag survives across a restart: the
	// HTTP listener below starts normally, but the dispatcher goroutine's
	// first Tick (and Run, right after it) sees draining still true and
	// exits immediately, so nothing is ever dispatched even though the
	// console comes up and serves normally (recovery-by-restart is the
	// intended path here per the plan's Q-runtime note, so "stopped" is
	// cleared too).
	if err = st.SetDraining(ctx, false); err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: clear draining flag: %w", err)
	}
	if err = st.SetStopped(ctx, false); err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: clear stopped flag: %w", err)
	}

	if seedDemo {
		if err = console.SeedDemo(ctx, st); err != nil {
			_ = st.Close()
			return fmt.Errorf("serve: seed demo: %w", err)
		}
	}

	bindings, err := ensureBindings(ctx, st, cfg.Projects, cfg.User)
	if err != nil {
		_ = st.Close()
		return err
	}

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		_ = st.Close()
		return err
	}

	// claude_oauth_token (PKG9-PLAN.md section 4.5, D26): checked right
	// after machine.toml loads, since config.Load itself never reads
	// machine.toml and so cannot require this on its own.
	if err = checkClaudeOAuthToken(m, cfg.ClaudeOAuthToken); err != nil {
		_ = st.Close()
		return err
	}

	// judge_codex_home (PKG9-PLAN.md section 4.5, 7.3, D27): resolved and
	// checked right here too, before the sandbox set loads, so the judge
	// profile never loads against a CODEX_HOME that cannot possibly be
	// right. dataDir is computed here, ahead of its other use further
	// down, because this check needs it too.
	//
	// Symlinks resolved once, right away, and every later use below takes
	// this same resolved value: sandbox.LoadProfile resolves DATA_DIR
	// internally for the profile's own literal SCENARIOS_FILE match
	// (macOS's own /var -> /private/var, review F044), but a judge run's
	// own writeScenariosFile hook (internal/job/judging.go) builds the file
	// it writes from Deps.DataDir verbatim; an unresolved dataDir here and
	// the sandbox's own resolved one would then disagree, and the judge's
	// own "zing scenarios" would fail inside the sandbox with "operation
	// not permitted" even though the file genuinely exists.
	rawDataDir := filepath.Dir(dbPath)
	dataDir, err := filepath.EvalSymlinks(rawDataDir)
	if err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: resolve data directory %s: %w", rawDataDir, err)
	}
	judgeCodexHome, err := resolveJudgeCodexHome(dataDir, cfg.JudgeCodexHome)
	if err != nil {
		_ = st.Close()
		return err
	}
	if err = checkJudgeCodexLogin(m, judgeCodexHome); err != nil {
		_ = st.Close()
		return err
	}

	// Production wires the two real runtimes, claude and codex, and nothing
	// else: no fake in production (design D2, section 4.1; task 14).
	// selftest and the dispatch/console e2e suites are the only remaining
	// callers that build a Fake, under all three machine.toml runtime names.
	rts, err := productionRuntimes(cfg.ClaudeOAuthToken)
	if err != nil {
		_ = st.Close()
		return err
	}

	// The GitHub tracker, from the configured token and one "owner/name" per
	// project (task 14, replacing the fixture tracker this used to build:
	// Package 6's D6 deferred this rewire here). The fixture tracker is now
	// built only from selftest and the test suites.
	tr, err := productionTracker(cfg)
	if err != nil {
		_ = st.Close()
		return err
	}

	floor, err := response.ParseSeverity(cfg.Review.Floor)
	if err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: %w", err)
	}

	// The seatbelt sandbox set every sandboxed job's run is wrapped in
	// (design section 5.5, 10, task 8; PKG9-PLAN.md section 4.7): loaded
	// once, here, and shared by every sandboxed job through
	// job.Deps.Sandboxes. serve always sets RequireSandbox true
	// (serveRequireSandbox, N9): an unavailable sandbox never runs a real
	// build or review unwrapped, it fails every sandboxed tick closed
	// instead (routeFailure's own ErrSandbox case).
	sbSet, err := serveSandbox(cfg, dataDir) //nolint:contextcheck // sandbox.Load's signature is fixed by PKG8-PLAN.md section 5.4 and carries no context.Context; the one exec.CommandContext call in its call chain (host_darwin.go) is bounded by its own fixed timeout instead
	if err != nil {
		_ = st.Close()
		return err
	}

	// The private temp root of every unsandboxed run (classify, planning,
	// planreview) lives under <DATA_DIR>/tmp/ (PKG9-PLAN.md section 7.3): a
	// process that died mid-run leaves its own run directory behind, so
	// serve sweeps the whole tree once, here, before the dispatcher starts.
	if err = removeStartupTempRoots(dataDir); err != nil {
		_ = st.Close()
		return err
	}

	// The per-run scenarios file every judge run writes (PKG9-PLAN.md
	// section 7.3, D19) lives under <DATA_DIR>/judge/: swept the same way,
	// before the dispatcher starts, since a dead process's own run id
	// folder is otherwise never cleaned up.
	if err = removeStartupJudgeDir(dataDir); err != nil {
		_ = st.Close()
		return err
	}

	// Each run's own stderr file (runjob.go's writeStderrFile) otherwise
	// accumulates forever under <DATA_DIR>/runs/: swept the same way, once
	// here before the dispatcher starts (ticket #8).
	removeStaleStderrFiles(ctx, st, dataDir, time.Now())

	gh, err := orchestrator.NewGitHubClient(cfg.GitHubToken)
	if err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: %w", err)
	}
	projects, err := buildJobProjects(ctx, cfg.Projects, bindings, gh, sbSet.Build)
	if err != nil {
		_ = st.Close()
		return err
	}

	// dispCtx is deliberately not derived from ctx's cancellation: the
	// drain sequence below stops the dispatcher through the store's
	// draining flag first, and only cancels dispCtx as the timeout
	// backstop, so a signal must not cancel it on its own.
	dispCtx, cancelDisp := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelDisp()

	d, err := zdispatch.New(st, tr, b, m, job.Registry(), bindings, zdispatch.Config{
		Interval:    dispatchInterval(cfg.Dispatch.IntervalSeconds),
		MaxParallel: dispatchMaxParallel(cfg.Dispatch.MaxParallel),
		Owner:       claimOwner(),
		Models: map[string]string{
			modelAliasSonnet: cfg.Models.Sonnet, modelAliasOpus: cfg.Models.Opus, modelAliasFable: cfg.Models.Fable, modelAliasCodex: cfg.Models.Codex,
		},
		Budget:         time.Duration(cfg.Budget.AgentMinutesPerTicket) * time.Minute,
		Floor:          floor,
		Projects:       projects,
		Sandboxes:      sbSet,
		RequireSandbox: serveRequireSandbox,
		Commands:       job.NewCommandRunner(sbSet.Build, serveRequireSandbox),
		DataDir:        dataDir,
		LensesParallel: cfg.Review.MaxLensesParallel,
		JudgeCodexHome: judgeCodexHome,
		MergeRule: job.MergeRule{
			Auto: cfg.Merge.Auto, Method: cfg.Merge.Method,
			ManualPaths: cfg.Merge.ManualPaths, DependencyFiles: cfg.Merge.DependencyFiles,
		},
		// ReclaimForeign is only safe once serve.lock proves every other
		// claim owner is dead (design section 6.2, 6.3): serve took that
		// lock above, so it is the only caller that ever sets this true.
		ReclaimForeign: true,
	}, rts)
	if err != nil {
		_ = st.Close()
		return err
	}

	// dispDone signals (by closing) once d.Run's goroutine has returned;
	// dispErr holds its return value, safe to read once dispDone has closed,
	// because the close happens-after the assignment in the same goroutine
	// and happens-before any receive of it (design section 6.10).
	dispDone := make(chan struct{})
	var dispErr error
	go func() {
		dispErr = d.Run(dispCtx)
		close(dispDone)
	}()

	// The bearer token GET /push/key and POST /push/subscribe check (design
	// section 6.13): an explicit console.push_token always wins; otherwise
	// the token persisted from an earlier run (or generated and persisted
	// now, on the very first run) is reused, so it is stable across
	// restarts.
	pushToken, err := resolvePushToken(ctx, st, cfg.Console.PushToken)
	if err != nil {
		_ = st.Close()
		return err
	}
	push := notify.New(st)

	// The mutation guard's Host allowlist (mw.go, design section 6.14):
	// every resolved bind authority plus every configured
	// console.allowed_hosts entry; console.New itself adds localhost and
	// 127.0.0.1 at the console port.
	allowedHosts := make([]string, 0, len(hosts)+len(cfg.Console.AllowedHosts))
	allowedHosts = append(allowedHosts, hosts...)
	allowedHosts = append(allowedHosts, cfg.Console.AllowedHosts...)

	handler := console.New(st, b, m, allowedHosts, cfg.Console.Port, logHandler, push, pushToken, floor, sbSet.FirstUnavailable(usedSandboxProfiles(m)), tr, cfg.User)
	srv := newServer(ctx, handler)

	listeners, err := listenOnAll(ctx, hosts, cfg.Console.Port)
	if err != nil {
		_ = st.Close()
		return err
	}

	// One srv.Serve(ln) goroutine per resolved listener (design section
	// 6.14: "One http.Server with one mux serves every resolved listener
	// through a srv.Serve(ln) goroutine each"). srv.Shutdown, below, closes
	// every listener registered this way on the same *http.Server.
	errCh := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func(ln net.Listener) { errCh <- srv.Serve(ln) }(ln)
	}
	slog.Info("starting", "hosts", hosts, "port", cfg.Console.Port)

	consumedFromErrCh, dispTriggered, serveErr := waitForShutdownTrigger(ctx, errCh, dispDone, func() error { return dispErr })

	return shutdown(ctx, st, srv, d, errCh, len(listeners), consumedFromErrCh, serveErr, dispTriggered, dispDone, func() error { return dispErr }, cancelDisp)
}

// waitForShutdownTrigger blocks until a real shutdown trigger arrives: an
// HTTP listener failure (errCh), a signal or parent cancellation
// (ctx.Done()), or the dispatcher's own goroutine ending with a nil error
// or context.Canceled (today's graceful-join case, which only happens if
// something else set draining). It returns what shutdown (below) needs:
// the listener error when that is what ended it, whether it was consumed
// from errCh, and dispTriggered, whether the dispatcher's own goroutine is
// what serve should name as the reason it stopped.
//
// A real dispatcher error (most commonly ErrFailClosed, design D3, section
// 4.7) does not end this wait on its own: the console already alerted
// (design section 4.6) and stays up so the owner can still use it, and this
// keeps waiting on errCh and ctx.Done() for one of the other two triggers.
// dispTriggered still ends up true once that later trigger arrives, so
// serve's eventual exit status still names the dispatcher's own failure
// (dispatchFailure, in shutdown below) -- the dispatcher merely stops being
// what ends serve's wait, not what serve blames for ending it. dispDoneCh
// is set to nil once that path is taken, so this same, already-closed
// channel is never selected again (a nil channel blocks forever, which is
// exactly "stop considering this case").
func waitForShutdownTrigger(ctx context.Context, errCh <-chan error, dispDone <-chan struct{}, dispErr func() error) (consumedFromErrCh, dispTriggered bool, serveErr error) {
	var dispFailedReal bool

	dispDoneCh := dispDone
selectLoop:
	for {
		select {
		case serveErr = <-errCh:
			consumedFromErrCh = true
			break selectLoop
		case <-ctx.Done():
			break selectLoop
		case <-dispDoneCh:
			if de := dispErr(); de != nil && !errors.Is(de, context.Canceled) {
				dispFailedReal = true
				dispDoneCh = nil
				continue selectLoop
			}
			dispTriggered = true
			break selectLoop
		}
	}
	if dispFailedReal {
		dispTriggered = true
	}
	return consumedFromErrCh, dispTriggered, serveErr
}

// serveRequireSandbox is always true in serve (design N9, section 10): a
// real build run must refuse to start when the sandbox did not load, no
// config key overrides it. selftest's own RequireSandbox is false
// (cmd/zing/selftest.go), never this constant.
const serveRequireSandbox = true

// serveSandbox loads the seatbelt profile set every sandboxed job's run is
// wrapped in (design section 5.1, 5.5, 10; PKG9-PLAN.md section 4.7): the
// checked-in, embedded sandbox/build.sb, sandbox/readonly.sb, and
// sandbox/judge.sb, each with cfg.Sandbox.ReadPaths and cfg.Console.Port.
// LoadProfile never errors -- a failure is recorded as unavailable, with
// one of section 5.4's four closed reasons -- so the only error this can
// return is reading an embedded profile itself, which would mean the
// binary was built without it.
func serveSandbox(cfg *config.Config, dataDir string) (sandbox.Set, error) {
	buildProfile, err := zing.Assets.ReadFile("sandbox/build.sb")
	if err != nil {
		return sandbox.Set{}, fmt.Errorf("serve: read embedded sandbox profile: %w", err)
	}
	readonlyProfile, err := zing.Assets.ReadFile("sandbox/readonly.sb")
	if err != nil {
		return sandbox.Set{}, fmt.Errorf("serve: read embedded sandbox profile: %w", err)
	}
	judgeProfile, err := zing.Assets.ReadFile("sandbox/judge.sb")
	if err != nil {
		return sandbox.Set{}, fmt.Errorf("serve: read embedded sandbox profile: %w", err)
	}

	build := loadNamedSandbox("build", buildProfile, cfg, dataDir)
	readonly := loadNamedSandbox("readonly", readonlyProfile, cfg, dataDir)
	judge := loadNamedSandbox("judge", judgeProfile, cfg, dataDir)

	return sandbox.Set{Build: build, ReadOnly: readonly, Judge: judge}, nil
}

// loadNamedSandbox loads one profile through sandbox.LoadProfile and logs
// whether it came up, naming the profile so serveSandbox's three calls are
// told apart in the log.
func loadNamedSandbox(name string, profile []byte, cfg *config.Config, dataDir string) sandbox.Sandbox {
	sb := sandbox.LoadProfile(name, profile, dataDir, cfg.Sandbox.ReadPaths, cfg.Console.Port)
	if sb.Available() {
		slog.Info("sandbox loaded", "profile", name)
	} else {
		slog.Error("sandbox unavailable", "profile", name, "reason", sb.Reason())
	}
	return sb
}

// usedSandboxProfiles returns the distinct, non-empty job.Sandbox names
// m.Jobs actually names, for Set.FirstUnavailable: a profile no job uses
// must never turn the console's sandbox indicator red (PKG9-PLAN.md
// section 4.7).
func usedSandboxProfiles(m *machine.Machine) []string {
	seen := make(map[string]bool, len(m.Jobs))
	var names []string
	for _, jobName := range sortedJobNames(m) {
		name := m.Jobs[jobName].Sandbox
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// sortedJobNames returns m.Jobs's keys in sorted order, so
// usedSandboxProfiles (and checkClaudeOAuthToken, below) walk machine.toml's
// jobs in a deterministic order rather than Go's randomized map order.
func sortedJobNames(m *machine.Machine) []string {
	names := make([]string, 0, len(m.Jobs))
	for name := range m.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// checkClaudeOAuthToken enforces claude_oauth_token's own requirement
// (PKG9-PLAN.md section 4.5, D26): when any machine.toml job uses the
// claude runtime and token is empty, it names the first such job in
// sorted order; nil otherwise. config.Load cannot make this check itself,
// since it never reads machine.toml.
func checkClaudeOAuthToken(m *machine.Machine, token string) error {
	if token != "" {
		return nil
	}
	for _, name := range sortedJobNames(m) {
		if m.Jobs[name].Runtime == runtimeNameClaude {
			return fmt.Errorf("serve: zing.toml: missing required key claude_oauth_token (machine.toml job %s uses the claude runtime)", name)
		}
	}
	return nil
}

// judgeDataDirSweptFolders are the two folders under DATA_DIR serve clears
// at startup (removeStartupTempRoots's own "tmp", and
// removeStartupJudgeDir's own "judge", PKG9-PLAN.md section 4.5, 7.3): a
// judge_codex_home under either would be removed out from under a running
// judge the moment serve restarts, so resolveJudgeCodexHome refuses both.
var judgeDataDirSweptFolders = []string{"tmp", "judge"}

// resolveJudgeCodexHome resolves judgeCodexHome's own symlinks (a missing
// folder -- the owner has not logged in there yet -- resolves its parent
// instead, then appends the folder's own base name back, since
// filepath.EvalSymlinks fails outright on a path that does not exist) and
// checks it names a real subfolder of dataDir (PKG9-PLAN.md section 4.5,
// D27): not dataDir itself, not a path outside it (a sibling whose name
// merely shares a prefix included, since filepath.Rel is what actually
// decides this, not a string-prefix check), and not inside either folder
// serve sweeps at startup. dataDir is serve's own resolved data directory
// (filepath.Dir(dbPath)); every build and readonly profile denies it
// whole, so a judge home anywhere else could leak the sealed scenarios a
// judge's own Codex session can quote to a build or review run.
func resolveJudgeCodexHome(dataDir, judgeCodexHome string) (string, error) {
	// dataDir is resolved too, not just judgeCodexHome: macOS's own /var ->
	// /private/var symlink (task 16a's own lesson, internal/sandbox) means
	// an unresolved dataDir and a resolved judgeCodexHome would otherwise
	// compare unequal paths that are really the same directory. dataDir
	// already exists (it is where zing.db lives), so a resolve failure
	// here is a real error, unlike judgeCodexHome's own "not logged in
	// yet" allowance below.
	resolvedDataDir, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return "", fmt.Errorf("serve: resolve data directory %s: %w", dataDir, err)
	}

	resolved, err := evalSymlinksAllowMissing(judgeCodexHome)
	if err != nil {
		return "", fmt.Errorf("serve: judge_codex_home %s: %w", judgeCodexHome, err)
	}

	rel, err := filepath.Rel(resolvedDataDir, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("serve: judge_codex_home %s must be inside the data directory %s", resolved, resolvedDataDir)
	}
	if rel == "." {
		return "", fmt.Errorf("serve: judge_codex_home %s must be a folder inside the data directory %s, not the data directory itself", resolved, resolvedDataDir)
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	if slices.Contains(judgeDataDirSweptFolders, first) {
		return "", fmt.Errorf("serve: judge_codex_home %s must not be inside %s, which serve clears at startup", resolved, filepath.Join(resolvedDataDir, first))
	}
	return resolved, nil
}

// evalSymlinksAllowMissing resolves path's symlinks like
// filepath.EvalSymlinks, except that a path which does not exist yet is
// not an error: its parent directory is resolved instead, and path's own
// base name is appended back. judge_codex_home's folder need not exist
// before this check runs (PKG9-PLAN.md section 4.5): the owner may not
// have run "codex login" there yet on a fresh install.
func evalSymlinksAllowMissing(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent, parentErr := filepath.EvalSymlinks(filepath.Dir(path))
	if parentErr != nil {
		return "", parentErr
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

// checkJudgeCodexLogin requires judgeCodexHome/auth.json to exist when
// machine.toml's own jobs.judge uses the codex runtime (PKG9-PLAN.md
// section 4.5): the owner logs in once with "CODEX_HOME=<judge_codex_home>
// codex login" before serve ever starts a real judge run.
func checkJudgeCodexLogin(m *machine.Machine, judgeCodexHome string) error {
	j, ok := m.Jobs["judge"]
	if !ok || j.Runtime != runtimeNameCodex {
		return nil
	}
	authPath := filepath.Join(judgeCodexHome, "auth.json")
	if _, err := os.Stat(authPath); err != nil {
		return fmt.Errorf("serve: judge Codex is not logged in: %s not found; run CODEX_HOME=%s codex login", authPath, judgeCodexHome)
	}
	return nil
}

// removeStartupTempRoots removes <dataDir>/tmp/ whole, before the
// dispatcher starts (PKG9-PLAN.md section 7.3): the private temp root an
// unsandboxed run's TMPDIR and CLAUDE_CODE_TMPDIR point at lives under it,
// and a process that died mid-run leaves its own run directory behind
// (runJob's own deferred cleanup only runs when rt.Run actually returns). A
// missing directory is not an error.
func removeStartupTempRoots(dataDir string) error {
	tmpRoot := filepath.Join(dataDir, "tmp")
	if err := os.RemoveAll(tmpRoot); err != nil {
		return fmt.Errorf("serve: remove %s: %w", tmpRoot, err)
	}
	return nil
}

// removeStartupJudgeDir removes <dataDir>/judge/ whole, before the
// dispatcher starts (PKG9-PLAN.md section 7.3, D19): each judge run's own
// afterReserve hook (internal/job/judging.go's writeScenariosFile) removes
// its own <run id> folder when rt.Run returns, but a process that dies
// mid-run skips that, so serve sweeps the whole tree here instead, the same
// way removeStartupTempRoots clears <dataDir>/tmp/. No judge run exists at
// startup, so nothing live is ever removed out from under it. A missing
// directory is not an error.
func removeStartupJudgeDir(dataDir string) error {
	judgeRoot := filepath.Join(dataDir, "judge")
	if err := os.RemoveAll(judgeRoot); err != nil {
		return fmt.Errorf("serve: remove %s: %w", judgeRoot, err)
	}
	return nil
}

// stderrRetention is how long a finished run's own stderr file
// (writeStderrFile, internal/job/runjob.go) survives before
// removeStaleStderrFiles deletes it.
const stderrRetention = 14 * 24 * time.Hour

// removeStaleStderrFiles deletes every <dataDir>/runs/run-<id>-stderr.log
// (writeStderrFile, internal/job/runjob.go) whose modification time is
// older than stderrRetention, unless its own run is still open (runs.
// outcome IS NULL, store.Store.OpenRunIDs), before the dispatcher starts
// (ticket #8): nothing else ever removes one of these files, so without
// this sweep <dataDir>/runs grows without bound. A file that matches the
// run-<id>-stderr.log shape but whose id fails to parse, or whose id names
// no open run, is treated as belonging to no open run, so it is removed on
// age alone; any file outside that shape is left alone regardless of age.
//
// It logs one INFO line with the count of files it actually removed, and a
// WARN line for every failure along the way (a failed open-run query or
// directory read, a failed per-file stat, or a failed remove), and never
// returns an error itself: a store or filesystem problem here is logged,
// not fatal, so it never keeps serve from starting (Q5).
func removeStaleStderrFiles(ctx context.Context, st *store.Store, dataDir string, now time.Time) int {
	dir := filepath.Join(dataDir, "runs")

	open, err := st.OpenRunIDs(ctx)
	if err != nil {
		slog.Warn("stderr retention skipped", "dir", dir, "err", err)
		return 0
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("stderr retention skipped", "dir", dir, "err", err)
			return 0
		}
		entries = nil
	}

	cutoff := now.Add(-stderrRetention)
	n := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		rest, ok := strings.CutPrefix(entry.Name(), "run-")
		if !ok {
			continue
		}
		idStr, ok := strings.CutSuffix(rest, "-stderr.log")
		if !ok {
			continue
		}
		id, perr := strconv.ParseInt(idStr, 10, 64)
		if perr == nil && open[id] {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		logArgs := []any{"path", path}
		if perr == nil {
			logArgs = append(logArgs, "run_id", id)
		}

		info, err := entry.Info()
		if err != nil {
			slog.Warn("stderr file stat failed", append(logArgs, "err", err)...)
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil {
			slog.Warn("stderr file remove failed", append(logArgs, "err", err)...)
			continue
		}
		n++
	}

	slog.Info("stderr files removed", "count", n)
	return n
}

// buildJobProjects builds one orchestrator.Orchestrator per configured
// project and returns job.Project keyed by its store project id (design
// section 10): owner and repo split from projects[i].repo, LocalPath from
// the project's own configured path, gh shared by every one of them, and
// BuildWritableRoots from the sandbox's own cache root and mds folder when
// it is available (task 8, design section 15). bindings supplies the store
// project id for each configured project name (ensureBindings, above). gh is
// the concrete *orchestrator.GitHubClient, not the four-method GitHub
// interface: every job.Project also carries Owner, Repo, and gh itself as
// its PullRequests, Flips, Checks, and Threads (PKG9-PLAN.md section 10.3),
// so the shipping and respond handlers read and write GitHub through the
// same client Package 5's git writes use.
func buildJobProjects(ctx context.Context, projects []config.Project, bindings []zdispatch.Binding, gh *orchestrator.GitHubClient, sb sandbox.Sandbox) (map[int64]job.Project, error) {
	storeProjectID := make(map[string]int64, len(bindings))
	for _, b := range bindings {
		storeProjectID[b.TrackerProject] = b.StoreProjectID
	}
	writableRoots := sandboxBuildWritableRoots(sb)

	out := make(map[int64]job.Project, len(projects))
	for i := range projects {
		p := &projects[i]
		id, ok := storeProjectID[p.Name]
		if !ok {
			return nil, fmt.Errorf("serve: project %s: no store binding", p.Name)
		}
		owner, repo, err := splitOwnerRepo(p.Repo)
		if err != nil {
			return nil, fmt.Errorf("serve: project %s: %w", p.Name, err)
		}
		defaultBranch := cmp.Or(p.DefaultBranch, "main")

		orch, err := orchestrator.New(orchestrator.Project{
			Owner: owner, Repo: repo, LocalPath: p.Path, DefaultBranch: defaultBranch,
			BuildWritableRoots: writableRoots,
		}, gh, orchestrator.NewRunner(), nil)
		if err != nil {
			return nil, fmt.Errorf("serve: project %s: build orchestrator: %w", p.Name, err)
		}
		repoGit, err := orch.GitCommonDir(ctx)
		if err != nil {
			return nil, fmt.Errorf("serve: project %s: git common dir: %w", p.Name, err)
		}
		out[id] = job.Project{
			Orch: orch, RepoGit: repoGit, TestCmd: p.Commands.Test, LintCmd: p.Commands.Lint,
			Owner: owner, Repo: repo, PullRequests: gh, Flips: gh, Checks: gh, Threads: gh,
		}
	}
	return out, nil
}

// sandboxBuildWritableRoots returns the two extra roots a sandboxed build
// run can write (design section 15): the sandbox's own cache root and the
// login's mds folder, read off Sandbox.ParamsFor -- the only way to reach
// those two host-only fields, since Sandbox carries no exported getter of
// its own. The other three ParamsFor arguments (worktree, repoGit, runDir)
// are irrelevant here and left empty. nil when the sandbox is unavailable:
// its host fields were never resolved, and Params fields would come back
// empty, which orchestrator.New would then refuse as a relative path.
func sandboxBuildWritableRoots(sb sandbox.Sandbox) []string {
	if !sb.Available() {
		return nil
	}
	p, err := sb.ParamsFor("", "", "")
	if err != nil {
		return nil
	}
	return []string{p.CacheRoot, p.MDSCache}
}

// productionRuntimes builds the runtime.Set serve wires the dispatcher with:
// the two real runtimes, claude and codex, resolved by their bare bin names
// (an empty bin argument to runtime.NewClaude/NewCodex resolves through
// PATH), and nothing else (design D2, section 4.1; task 14). oauthToken is
// cfg.ClaudeOAuthToken, the only credential the claude runtime ever carries
// to its child (PKG9-PLAN.md section 4.6, D26). Production never maps the
// fake name; only selftest and the test suites still build a runtime.Fake.
func productionRuntimes(oauthToken string) (runtime.Set, error) {
	rts, err := runtime.NewSet(map[string]runtime.Runtime{
		runtimeNameClaude: runtime.NewClaude("", oauthToken),
		runtimeNameCodex:  runtime.NewCodex(""),
	})
	if err != nil {
		return runtime.Set{}, fmt.Errorf("serve: %w", err)
	}
	return rts, nil
}

// productionTracker builds the GitHub tracker serve wires the dispatcher
// with (task 14, replacing the fixture tracker): cfg.GitHubToken authenticates
// the one client every configured project shares, and repos maps each
// project's name to its own "owner/name" -- config.Project.Repo is already
// stored in that exact shape ("zing project add"'s splitOwnerRepo validates
// it on the way in; cmd/zing/project.go), so no parsing happens here. The
// token is read once from cfg and passed straight into tracker.NewGitHub;
// it is never logged and never reaches a job's environment (the runtimes'
// own env filter already drops every *_TOKEN name by construction).
func productionTracker(cfg *config.Config) (*tracker.GitHubTracker, error) {
	repos := make(map[string]string, len(cfg.Projects))
	for i := range cfg.Projects {
		repos[cfg.Projects[i].Name] = cfg.Projects[i].Repo
	}
	tr, err := tracker.NewGitHub(cfg.GitHubToken, repos)
	if err != nil {
		return nil, fmt.Errorf("serve: %w", err)
	}
	return tr, nil
}

// listenOnAll opens one TCP listener per host in hosts, each at port. On
// any failure it closes every listener already opened before returning the
// error, so a mid-list bind failure leaks no socket.
func listenOnAll(ctx context.Context, hosts []string, port int) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(hosts))
	for _, host := range hosts {
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("listen on %s:%d: %w", host, port, err)
		}
		listeners = append(listeners, ln)
	}
	return listeners, nil
}

// resolvePushToken resolves the console's bearer token for GET /push/key
// and POST /push/subscribe (design section 6.13): an explicitly configured
// console.push_token always wins, so rotating it in zing.toml takes effect
// on the next restart; otherwise the token persisted in settings.push_token
// from an earlier run is reused, and when neither exists yet (the very
// first run, with no explicit token) a fresh one is generated and persisted
// once, so it is then stable across every later restart. config.Load never
// invents a token of its own (internal/config/config.go), which is what
// makes "explicit" and "generated" distinguishable here: configured is
// empty unless zing.toml set console.push_token.
func resolvePushToken(ctx context.Context, st *store.Store, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}

	stored, ok, err := st.GetSetting(ctx, settingPushToken)
	if err != nil {
		return "", fmt.Errorf("get %s setting: %w", settingPushToken, err)
	}
	if ok && stored != "" {
		return stored, nil
	}

	token, err := generateBearerToken()
	if err != nil {
		return "", fmt.Errorf("generate push token: %w", err)
	}
	if err := st.SetSettings(ctx, settingPushToken, token); err != nil {
		return "", fmt.Errorf("persist push token: %w", err)
	}
	return token, nil
}

// settingPushToken is the settings.key resolvePushToken persists a
// generated token under (design section 6.13, 14: "settings.push_token").
const settingPushToken = "push_token"

// generateBearerToken returns a fresh random bearer token, 32 bytes of
// crypto/rand encoded as base64url (the same shape
// internal/config's now-removed generatePushToken used).
func generateBearerToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// shutdown runs the drain-then-close sequence (design section 6.10 step
// 10) through drainAndShutdown, then folds in the things that sequence does
// not carry through its channel-of-struct{} and closure shape: the
// dispatcher's own returned error and the console listener's error from
// errCh. It returns the first real error among: the dispatcher (only when
// dispTriggered, i.e. the dispatcher's own goroutine, not a signal or an
// HTTP failure, is what ended serve's select), the console listener,
// Shutdown, and the store close. d is used only to wake Run promptly once
// draining is set (d.NotifyDrain, called from the setDraining closure
// below); drainAndShutdown itself stays decoupled from
// *dispatch.Dispatcher; so does shutdown_test.go, which drives it directly.
func shutdown(
	ctx context.Context, st *store.Store, srv *http.Server, d *zdispatch.Dispatcher,
	errCh <-chan error, listenerCount int, consumedFromErrCh bool,
	serveErr error, dispTriggered bool, dispDone <-chan struct{}, dispErr func() error, cancelDisp context.CancelFunc,
) error {
	err := drainAndShutdown(
		ctx, drainDeadline,
		func() error {
			setErr := st.SetDraining(context.WithoutCancel(ctx), true)
			if setErr == nil {
				// Wake Run promptly rather than leaving it to notice only
				// on the next ticker fire, which can race a short drain
				// deadline (design section 6.10; dispatch.Dispatcher's own
				// NotifyDrain doc comment).
				d.NotifyDrain()
			}
			return setErr
		},
		dispDone,
		cancelDisp,
		func(parent context.Context) error {
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), shutdownTimeout)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		},
		st.Close,
	)

	// dispDone is guaranteed closed by the time drainAndShutdown returns, so
	// dispErr() is safe to read here.
	de := dispErr()
	if de != nil && !errors.Is(de, context.Canceled) {
		slog.Error("dispatcher stopped", "err", de)
	}

	serveErr = resolveServeErr(serveErr, dispTriggered, de, err)
	// Every srv.Serve(ln) goroutine (one per resolved listener) sends its
	// own return value to errCh; the outer select above already consumed
	// one of them when consumedFromErrCh is true. Drain the rest here, so
	// none of those goroutines blocks forever on a full send, and keep the
	// first error that is not the expected http.ErrServerClosed a clean
	// Shutdown produces.
	remaining := listenerCount
	if consumedFromErrCh {
		remaining--
	}
	for range remaining {
		if e := <-errCh; e != nil && !errors.Is(e, http.ErrServerClosed) && serveErr == nil {
			serveErr = e
		}
	}
	return serveErr
}

// dispatchFailure decides whether the dispatcher's own captured error (de)
// should become serve's return value. It fires only when dispTriggered is
// true, meaning the dispatcher's goroutine, rather than a signal or an HTTP
// listener failure, is what ended serve's main select: today a dispatcher
// error (for example dispatch.ErrFailClosed) stops all intake while HTTP
// keeps serving, and serve never reports it. A nil error, or
// context.Canceled (the expected result of the drain sequence's own forced
// cancel), never counts as a failure.
func dispatchFailure(dispTriggered bool, de error) error {
	if !dispTriggered || de == nil || errors.Is(de, context.Canceled) {
		return nil
	}
	return fmt.Errorf("dispatcher: %w", de)
}

// resolveServeErr picks shutdown's own return value from every error
// source it collects (PR review fix D3): triggerErr is whatever ended
// waitForShutdownTrigger's own select (a listener failure from errCh, or
// nil for a signal or a benign dispDone); dispTriggered and de are
// waitForShutdownTrigger's own report of the dispatcher's goroutine; err is
// drainAndShutdown's own shutdown/closeStore error.
//
// dispatchFailure(dispTriggered, de) always wins when it is non-nil. Before
// this fix, a dispatcher failure was reported only when triggerErr was
// still nil by the time shutdown ran its own "if serveErr == nil" check --
// so a listener failure that happened to end the wait after the dispatcher
// had already failed closed (waitForShutdownTrigger's own documented case:
// the dispatcher's failure keeps the wait going until a real trigger
// arrives) silently masked the dispatcher's own failure instead of joining
// or naming it. dispTriggered being true already means the dispatcher's
// failure is what the console alerted on and what an operator needs named
// in serve's own exit status, whatever else also happened to end the wait.
func resolveServeErr(triggerErr error, dispTriggered bool, de, drainErr error) error {
	if dispFail := dispatchFailure(dispTriggered, de); dispFail != nil {
		return dispFail
	}
	if triggerErr != nil {
		return triggerErr
	}
	return drainErr
}

// drainAndShutdown runs the section 6.10 drain-then-close sequence, decoupled
// from *store.Store and *http.Server so it can be driven directly in a test:
// it marks the system draining, waits for the dispatcher to signal dispDone,
// bounded by drainDeadline; if dispDone has not closed by then, it logs
// "drain timed out", calls forceDisp to cancel the dispatcher's own context,
// and waits again without a bound, because the store must never close while
// a handler may still be running. Only once dispDone has closed does it call
// shutdown and, last, closeStore. It returns the first error from shutdown or
// closeStore; a setDraining error is logged, not returned, since drain and
// join must proceed regardless (design section 6.10).
func drainAndShutdown(
	ctx context.Context,
	drainDeadline time.Duration,
	setDraining func() error,
	dispDone <-chan struct{},
	forceDisp context.CancelFunc,
	shutdown func(context.Context) error,
	closeStore func() error,
) error {
	if err := setDraining(); err != nil {
		slog.Error("set draining", "err", err)
	}

	select {
	case <-dispDone:
	case <-time.After(drainDeadline):
		slog.Error("drain timed out")
		forceDisp()
		<-dispDone
	}

	var err error
	if shutErr := shutdown(ctx); shutErr != nil {
		err = shutErr
	}
	if closeErr := closeStore(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}

// installLogHandler builds the Task 5 slog.Handler (internal/console/log.go,
// design section 6.12), seeds its LevelVar from settings.log_level, and
// installs it as slog's process-wide default, so every slog call from here
// on -- this package's own and every other package's -- goes through the
// one handler console.New's Task 10 log argument wires into POST /loglevel,
// POST /debug, and the rail's Log tail. Writing to os.Stderr, the same sink
// slog's own factory default uses, preserves this process's existing log
// output shape; only the level gate, the per-ticket debug override, and the
// ring are new. A missing or unrecognized stored level (a hand-edited
// settings row, or a fresh database before migrations seed it -- store.Open
// always runs them first, so this is defensive, not an expected path)
// defaults to info and is logged once, rather than failing serve over a bad
// setting.
//
// b.Publish is wired in as onWarn (design section 6a): a fresh WARN-or-above
// record wakes every open console /stream so the #alerts region patches
// live, the same bus every store-changing handler already publishes to.
//
// This is a bounded amplification, accepted rather than gated by source
// package: a single WARN anywhere in the process wakes every open /stream,
// and a per-tab patch-failure slog.Warn re-renders all four regions of
// every other healthy stream. Each cycle still terminates, since a failing
// stream exits after one frame, so it cannot compound across warnings. For
// a single-user loopback/tailnet console this fan-out is fine; it would
// need reconsidering before this handler served multiple concurrent users.
func installLogHandler(ctx context.Context, st *store.Store, b *bus.Broker) (*console.Handler, error) {
	lv := new(slog.LevelVar)
	h := console.NewHandler(os.Stderr, lv, b.Publish)
	slog.SetDefault(slog.New(h))

	stored, ok, err := st.GetSetting(ctx, "log_level")
	if err != nil {
		return nil, fmt.Errorf("serve: get log_level setting: %w", err)
	}
	level, known := console.ParseLogLevel(stored)
	if !ok || !known {
		slog.Warn("settings.log_level missing or unrecognized, defaulting to info", "stored", stored)
		level = slog.LevelInfo
	}
	lv.Set(level)

	return h, nil
}

// maxDispatchIntervalSeconds is the largest interval_seconds value that
// time.Duration(seconds) * time.Second cannot overflow an int64 nanosecond
// count (cubic P1): dispatchInterval passes its result straight to
// time.NewTicker inside the dispatcher's goroutine, so a value above this
// would wrap around to a bogus (often negative) duration and panic it.
const maxDispatchIntervalSeconds = math.MaxInt64 / int64(time.Second)

// dispatchInterval returns the dispatcher's tick interval for a configured
// dispatch.interval_seconds, clamping a non-positive value (zero or
// negative, whether from an explicit zing.toml entry or an unset field) and
// a value large enough to overflow a time.Duration
// (maxDispatchIntervalSeconds) to defaultDispatchInterval. Passed straight
// through to time.Ticker, either an out-of-range value would otherwise
// panic it.
func dispatchInterval(seconds int) time.Duration {
	switch {
	case seconds <= 0:
		slog.Warn("dispatch.interval_seconds is not positive, using the default",
			"interval_seconds", seconds, "default_seconds", int(defaultDispatchInterval.Seconds()))
		return defaultDispatchInterval
	case int64(seconds) > maxDispatchIntervalSeconds:
		slog.Warn("dispatch.interval_seconds overflows a time.Duration, using the default",
			"interval_seconds", seconds, "default_seconds", int(defaultDispatchInterval.Seconds()))
		return defaultDispatchInterval
	default:
		return time.Duration(seconds) * time.Second
	}
}

// dispatchMaxParallel returns the dispatcher's max-parallel guard for a
// configured dispatch.max_parallel, clamping a non-positive value to
// defaultDispatchMaxParallel. Tick's max-parallel guard (design section 6.8
// step 4) is "active >= max_parallel"; a non-positive value would make that
// guard true before any ticket is ever claimed, stalling all processing.
func dispatchMaxParallel(n int) int {
	if n <= 0 {
		slog.Warn("dispatch.max_parallel is not positive, using the default",
			"max_parallel", n, "default", defaultDispatchMaxParallel)
		return defaultDispatchMaxParallel
	}
	return n
}

// ensureBindings ensures a store project for every configured project and
// returns the dispatch.Binding each one needs for intake (design section
// 6.10 step 3). user is the configured user Zing acts for (cfg.User), set
// on every Binding.User so intake can name it in the pickup comment; it is
// cfg.User, not a project's own intake assignee, since the assignee is only
// a filter and the person Zing represents is the configured user (plan
// section 6).
func ensureBindings(ctx context.Context, st *store.Store, projects []config.Project, user string) ([]zdispatch.Binding, error) {
	bindings := make([]zdispatch.Binding, 0, len(projects))
	for i := range projects {
		p := &projects[i]
		id, err := st.EnsureProject(ctx, store.Project{
			Name:          p.Name,
			RepoURL:       p.Repo,
			LocalPath:     p.Path,
			Tracker:       p.Tracker,
			DefaultBranch: p.DefaultBranch,
		})
		if err != nil {
			return nil, fmt.Errorf("ensure project %s: %w", p.Name, err)
		}
		bindings = append(bindings, zdispatch.Binding{
			StoreProjectID: id,
			TrackerProject: p.Name,
			Rule:           tracker.IntakeRule{Assignee: p.Intake.AssignedTo},
			User:           user,
			Mode:           p.Intake.Mode,
		})
	}
	return bindings, nil
}

// claimOwnerNonce is 8 lowercase hex characters from crypto/rand, minted
// once per process (design section 6.2): a new serve that reuses a dead
// serve's PID after a reboot must never mistake the old serve's claims for
// its own just because the hostname and PID happen to match again. Nothing
// ever parses the owner string; the nonce only needs to differ from one
// process's lifetime to the next.
var claimOwnerNonce = sync.OnceValue(func() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is effectively unreachable on every platform
		// this builds for; a fixed fallback keeps claimOwner() returning a
		// usable id instead of panicking, at the cost of this one
		// process's own extra collision guard for this one run.
		return "00000000"
	}
	return hex.EncodeToString(buf)
})

// claimOwner returns this process's claim owner id,
// <hostname>-<pid>-<nonce> (design section 6.2, 7.2).
func claimOwner() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), claimOwnerNonce())
}

// newServer builds the HTTP server with its timeouts and graceful-drain
// wiring. drainCtx carries ctx's values (none, today) but never its
// cancellation, so a caller's ctx (the signal-derived base context) cannot
// tear down in-flight requests out from under Shutdown; only Shutdown's own
// RegisterOnShutdown callback cancels it.
//
// Shutdown closes listeners and waits for handlers, but it does not cancel
// request contexts on its own. Every request context here derives from a
// drain context that is cancelled when Shutdown begins, so long-lived SSE
// handlers that select on r.Context().Done() exit instead of holding
// Shutdown until its deadline.
//
// WriteTimeout stays unset on purpose: SSE handlers hold the connection
// open. Non-streaming routes should bound writes with http.ResponseController
// instead.
func newServer(ctx context.Context, handler http.Handler) *http.Server {
	drainCtx, drain := context.WithCancel(context.WithoutCancel(ctx))
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return drainCtx },
	}
	srv.RegisterOnShutdown(drain)
	return srv
}
