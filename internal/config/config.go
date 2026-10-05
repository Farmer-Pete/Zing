// Package config loads the user/install config at ~/.zing/zing.toml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

type Config struct {
	User string `toml:"user"`
	// GitHubToken authenticates the orchestrator's go-github client
	// (internal/orchestrator.NewGitHub). Required; never logged, never
	// written to another file (PKG5-PLAN.md section 9 and 14).
	GitHubToken string `toml:"github_token"`
	// ClaudeOAuthToken authenticates every claude-runtime run (PKG9-PLAN.md
	// section 4.5, D26): the output of `claude setup-token`, carried to
	// runtime.NewClaude alone and never logged, matching github_token's own
	// rule (non-empty, no minimum length). serve requires it, right after
	// machine.toml loads, when any job's runtime is "claude"; config.Load
	// itself cannot enforce that, since it never reads machine.toml.
	ClaudeOAuthToken string `toml:"claude_oauth_token"`
	// JudgeCodexHome is judge_codex_home (PKG9-PLAN.md section 4.5, D27):
	// the judge's own persistent Codex home, so its login and session
	// files survive across runs instead of a fresh home each time. `~`
	// expanded here; default "~/.zing/codex-judge", also expanded. Must be
	// absolute after expansion. serve alone checks it sits inside DATA_DIR
	// and holds auth.json (config.Load never reads machine.toml or
	// DATA_DIR, so it cannot make either check itself).
	JudgeCodexHome string     `toml:"judge_codex_home"`
	Console        Console    `toml:"console"`
	Models         Models     `toml:"models"`
	Dispatch       Dispatch   `toml:"dispatch"`
	Budget         Budget     `toml:"budget"`
	Review         Review     `toml:"review"`
	Merge          Merge      `toml:"merge"`
	Sandbox        Sandbox    `toml:"sandbox"`
	ReviewBots     ReviewBots `toml:"review_bots"`
	Projects       []Project  `toml:"projects"`
}

// Sandbox is the [sandbox] table (PKG8-PLAN.md section 5.4): ReadPaths
// extends the seatbelt profile's own home-read allow list, for a toolchain
// installed under home.
type Sandbox struct {
	ReadPaths []string `toml:"read_paths"`
}

type Console struct {
	Bind []string `toml:"bind"`
	Port int      `toml:"port"`
	// PushToken is left as the toml zero value (empty) by Load and
	// LoadForAdd when zing.toml omits it (see Load's doc comment).
	PushToken string `toml:"push_token"`
	// AllowedHosts extends the mutation middleware's Host allowlist (design
	// section 6.14) with hostnames the middleware cannot derive on its own,
	// such as a tailnet DNS name: bare hostnames, no port. Empty by default.
	AllowedHosts []string `toml:"allowed_hosts"`
}

type Models struct {
	Sonnet string `toml:"sonnet"`
	Opus   string `toml:"opus"`
	Fable  string `toml:"fable"`
	Codex  string `toml:"codex"`
}

type Dispatch struct {
	IntervalSeconds int `toml:"interval_seconds"`
	MaxParallel     int `toml:"max_parallel"`
}

type Budget struct {
	AgentMinutesPerTicket int `toml:"agent_minutes_per_ticket"`
	UsageHoldPercent      int `toml:"usage_hold_percent"`
}

type Review struct {
	Floor string `toml:"floor"`
	// MaxLensesParallel bounds how many of ROUND's seven lens runs are ever
	// in flight at once (PKG9-PLAN.md section 4.5): 1 to 7, default 7 (the
	// lens count; a higher value would only idle).
	MaxLensesParallel int `toml:"max_lenses_parallel"`
}

type Merge struct {
	Auto            bool     `toml:"auto"`
	Method          string   `toml:"method"`
	ManualPaths     []string `toml:"manual_paths"`
	DependencyFiles []string `toml:"dependency_files"`
}

// ReviewBots is the [review_bots] table: a required check that belongs to a
// review bot (CodeRabbit by default) can stop reporting when the bot is
// rate-limited, leaving POLL waiting forever. WaitMinutes is how long a
// configured check may sit missing before pollIdle posts that check's
// Trigger comment once, and again that long before it escalates to the
// owner (internal/job/shiprules.go's reviewBotAction).
type ReviewBots struct {
	WaitMinutes int              `toml:"wait_minutes"`
	Checks      []ReviewBotCheck `toml:"checks"`
}

// ReviewBotCheck names one required check that belongs to a review bot
// (Check, matched against a PR's missing required checks by equality) and
// the comment pollIdle posts to nudge it (Trigger, posted verbatim).
type ReviewBotCheck struct {
	Check   string `toml:"check"`
	Trigger string `toml:"trigger"`
}

type Project struct {
	Name    string `toml:"name"`
	Repo    string `toml:"repo"`
	Path    string `toml:"path"`
	Tracker string `toml:"tracker"`
	// DefaultBranch is the branch the orchestrator worktrees off of and
	// targets a draft PR at. Optional; left "" when zing.toml omits it (not
	// defaulted to "main" here, so a real default branch recorded earlier
	// is never overwritten by a defaulted one -- see applyDefaults).
	// ensureBindings (cmd/zing/serve.go) passes it into store.EnsureProject,
	// which defaults ""->"main" on INSERT, skips its reconcile of
	// DefaultBranch entirely when the value is "", and otherwise reconciles
	// a changed value into the projects table on every start
	// (store/spine.go).
	DefaultBranch string   `toml:"default_branch"`
	Self          bool     `toml:"self"`
	Intake        Intake   `toml:"intake"`
	Commands      Commands `toml:"commands"`
}

type Intake struct {
	AssignedTo string `toml:"assigned_to"`
	// Mode is "auto" (default) or "manual" (PKG9-PLAN.md D29): auto keeps
	// today's rule, open issues assigned to AssignedTo; manual means the
	// dispatcher never calls Tracker.Intake for this project, and
	// AssignedTo is not required -- applyDefaults only fills AssignedTo
	// from the top-level user when Mode is auto. Any other explicit value
	// is a load error (checkValues).
	Mode string `toml:"mode"`
}

// IntakeModeAuto and IntakeModeManual are intake.mode's two valid explicit
// values (PKG9-PLAN.md D29). An absent mode defers to applyDefaults, which
// fills IntakeModeAuto.
const (
	IntakeModeAuto   = "auto"
	IntakeModeManual = "manual"
)

// validIntakeModes is checkValues' own allowlist for an explicit
// intake.mode value, built from the two constants above the same way
// validReviewFloors and validMergeMethods list their own fields' allowed
// values.
var validIntakeModes = []string{IntakeModeAuto, IntakeModeManual}

type Commands struct {
	Test string `toml:"test"`
	Lint string `toml:"lint"`
	// Fix is projects[i].commands.fix: an optional, owner-set shell command
	// run verbatim by /bin/sh -c under the build sandbox, trusted the same
	// as Test and Lint. CHECK runs it before Lint when it is set; empty (the
	// default) means CHECK runs no fix step. Load applies no validation to
	// it, the same as Test and Lint. "omitempty" keeps AppendProject from
	// writing an explicit fix = "" for a project that never set one.
	Fix string `toml:"fix,omitempty"`
}

// DefaultPath returns the default config path, ~/.zing/zing.toml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home dir: %w", err)
	}
	return filepath.Join(home, ".zing", "zing.toml"), nil
}

var (
	validReviewFloors = []string{"blocker", "major", "minor", "nit"}
	validMergeMethods = []string{"squash", "merge", "rebase"}
)

// minPushTokenLen is the shortest console.push_token Load accepts when
// zing.toml sets one explicitly (design section 6.13's bearer token gates
// every POST /subscribe and /push route), counted in runes
// (utf8.RuneCountInString) to match the "16 characters" wording in the error
// message below -- a byte-length check under-counts a multi-byte character
// as more than one toward the floor, and over-counts a rune-short token that
// happens to use multi-byte characters as long enough. It is not applied to
// the auto-generated token cmd/zing/serve.go creates and persists to
// settings.push_token when zing.toml sets none; that value is generated at
// a fixed, already-adequate length, so this floor only catches a
// hand-written value weak enough to downgrade push auth toward a guessable
// bearer token.
const minPushTokenLen = 16

// minBudgetMinutes and maxBudgetMinutes bound budget.agent_minutes_per_ticket
// (design section 4.4, D5): 1 minute at the floor, 525600 (60*24*365, one
// non-leap year) at the ceiling. The ceiling also keeps
// time.Duration(minutes)*time.Minute (job.Deps.Budget) well clear of
// int64 nanosecond overflow.
const (
	minBudgetMinutes = 1
	maxBudgetMinutes = 525600
)

// minLensesParallel and maxLensesParallel bound
// review.max_lenses_parallel (design section 4.5): 1 at the floor, 7 at
// the ceiling -- the number of lenses ROUND runs, above which a higher
// value would only idle.
const (
	minLensesParallel = 1
	maxLensesParallel = 7
)

// minReviewBotWaitMinutes and maxReviewBotWaitMinutes bound
// review_bots.wait_minutes: 1 minute at the floor, 1440 (one day) at the
// ceiling.
const (
	minReviewBotWaitMinutes = 1
	maxReviewBotWaitMinutes = 1440
)

// defaultReviewBotWaitMinutes and defaultReviewBotChecks are review_bots'
// own defaults when the table, or just the checks key, is absent: a
// 20-minute wait, and CodeRabbit's own trigger comment.
const defaultReviewBotWaitMinutes = 20

var defaultReviewBotChecks = []ReviewBotCheck{{Check: "CodeRabbit", Trigger: "@coderabbitai review"}}

// Load reads and validates the zing.toml at path, in this exact order so the
// first reported error is deterministic: mode repair, decode, unknown-key
// check, missing-required check, value checks, then defaults. It requires at
// least one project; LoadForAdd is the same load with that one requirement
// relaxed. Console.PushToken is left empty when zing.toml omits it (design
// section 6.13): cmd/zing/serve.go is the one place that resolves the
// effective bearer token, since only it can tell an explicit zing.toml value
// apart from a value that needs to be generated once and persisted to
// settings.push_token for stability across restarts -- a distinction a value
// regenerated fresh on every Load call could not preserve.
func Load(path string) (*Config, error) {
	return load(path, false)
}

// LoadForAdd loads config for "zing project add" only (PKG5-PLAN.md section
// 9). It runs the same decode, unknown-key, and value checks as Load, but
// permits zero projects, so the first project can be added to a fresh
// config. It still requires user and github_token.
func LoadForAdd(path string) (*Config, error) {
	return load(path, true)
}

// load is the shared decode/unknown-key/value-check body of Load and
// LoadForAdd. allowEmptyProjects is their one difference.
func load(path string, allowEmptyProjects bool) (*Config, error) {
	if err := repairFileMode(path); err != nil {
		return nil, err
	}

	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("zing.toml: %w", err)
	}

	if err := checkUnknownKeys(md); err != nil {
		return nil, err
	}
	if err := checkRequiredKeys(cfg, allowEmptyProjects); err != nil {
		return nil, err
	}
	// Resolved before checkValues, which needs to see judge_codex_home
	// already ~-expanded: an explicit "~/..." value must be checked for
	// being absolute only after expansion, and the error it reports should
	// name the expanded path, not the literal "~" zing.toml wrote.
	if err := resolveJudgeCodexHome(md, &cfg); err != nil {
		return nil, err
	}
	if err := checkValues(md, cfg); err != nil {
		return nil, err
	}

	applyDefaults(md, &cfg)

	return &cfg, nil
}

// defaultJudgeCodexHomeRel is judge_codex_home's own default, relative to
// the user's home directory (PKG9-PLAN.md section 4.5, D27): "~/.zing/codex-judge".
const defaultJudgeCodexHomeRel = ".zing/codex-judge"

// resolveJudgeCodexHome fills cfg.JudgeCodexHome: an explicit value gets
// its leading "~" expanded (expandHome), the default
// "~/.zing/codex-judge" otherwise, already expanded, so both land
// absolute (PKG9-PLAN.md section 4.5). checkValues' own absolute check
// below only ever sees an explicit value, since the default is
// constructed absolute here.
func resolveJudgeCodexHome(md toml.MetaData, cfg *Config) error {
	if md.IsDefined("judge_codex_home") {
		expanded, err := expandHome(cfg.JudgeCodexHome)
		if err != nil {
			return fmt.Errorf("zing.toml: judge_codex_home: %w", err)
		}
		cfg.JudgeCodexHome = expanded
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("zing.toml: judge_codex_home: resolve home directory: %w", err)
	}
	cfg.JudgeCodexHome = filepath.Join(home, filepath.FromSlash(defaultJudgeCodexHomeRel))
	return nil
}

// expandHome replaces a leading "~" (exactly "~", or "~/..." ) in path with
// the user's home directory, the one tilde-expansion shape zing.toml ever
// needs (no "~user" form). A path with no leading "~" is returned
// unchanged.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// permissiveMode is the bit set that makes a file group- or other-readable.
// zing.toml carries github_token, so a file with any of these bits set gets
// repaired to 0600 (PKG5-PLAN.md section 9).
const permissiveMode = 0o077

// repairFileMode chmods path to 0600 when it is readable by group or other,
// logging a warning once. It runs first, before the decode, so a token is
// never left group- or other-readable past the start of a Load or
// LoadForAdd call, even one that goes on to fail a later check. It chmods
// only a regular file (PR review fix): path pointing at a directory or some
// other non-regular file (a symlink to one, a device, ...) is left alone and
// reported as an error, rather than chmod'd to a mode that could make a
// directory unusable (0600 strips the execute bit a directory needs to be
// traversable).
func repairFileMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("zing.toml: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("zing.toml: %s: not a regular file", path)
	}
	if info.Mode().Perm()&permissiveMode == 0 {
		return nil
	}
	slog.Warn("zing.toml is readable by group or other; repairing to 0600", "path", path, "mode", info.Mode().Perm())
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("zing.toml: repair file mode: %w", err)
	}
	return nil
}

// emptyProjectsArrayPattern matches a pre-existing explicit "projects = []"
// key assignment -- the bootstrap-with-zero-projects shape LoadForAdd
// permits -- in every realistic spelling: "projects = []", "projects=[]",
// extra internal spacing, and the multiline empty form "projects = [\n]".
// It anchors "projects" to right after a line's leading whitespace, so it
// can never match a "[[projects]]" table header (which starts with "[[",
// not "projects"), a key that merely contains "projects" as a substring
// (such as "other_projects"), or a value that happens to contain the word.
var emptyProjectsArrayPattern = regexp.MustCompile(`(?m)^[ \t]*projects[ \t]*=[ \t]*\[[ \t\r\n]*\][ \t]*\r?\n?`)

// AppendProject appends a single [[projects]] block for p to the zing.toml
// already at path, atomically and at mode 0600, since the file holds
// github_token (PKG5-PLAN.md section 9). It never re-marshals or rewrites
// anything already in the file (PR review fix): an earlier version of "zing
// project add" re-marshaled and rewrote the whole Config, which meant any
// field the caller had set to its Go zero value on purpose -- an explicit
// merge.manual_paths = [] or budget.usage_hold_percent = 0 -- came back out
// of that Config struct indistinguishable from a field zing.toml had simply
// never mentioned, and so was silently dropped by the rewrite. Rendering
// only the new project and appending it leaves every existing key and value
// byte-for-byte untouched, so no explicit empty or zero value already on
// disk is ever at risk.
//
// The new block is produced by marshaling a small wrapper struct holding
// only p, so BurntSushi/toml's own TOML-string escaping applies to it
// exactly as it would to the full Config. It is written to a fresh sibling
// temp file made with os.CreateTemp(dir, "zing.toml.*.tmp") -- a unique name
// per call, so a stale leftover temp file from an earlier interrupted or
// crashed append can never block this one -- chmod'd to 0600, written,
// closed, and renamed over path. On any error the temp file is removed and
// path is left untouched, since the rename never runs until every earlier
// step has succeeded.
//
// Before appending, it strips a pre-existing explicit empty "projects = []"
// key assignment, if the file has one (PR review fix): that is exactly the
// bootstrap-with-zero-projects shape LoadForAdd permits, and appending
// "[[projects]]" onto a file that already defines "projects" as an inline
// empty array redefines the same key twice, which the next Load then
// rejects outright ("Key 'projects' was already created and cannot be used
// as an array"). Removing that inline assignment first makes the appended
// block the first element instead.
func AppendProject(path string, p Project) error {
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("zing.toml: append project: %w", err)
	}
	existing = emptyProjectsArrayPattern.ReplaceAll(existing, nil)

	wrapper := struct {
		Projects []Project `toml:"projects"`
	}{[]Project{p}}
	block, err := toml.Marshal(wrapper)
	if err != nil {
		return fmt.Errorf("zing.toml: append project: %w", err)
	}

	var out bytes.Buffer
	out.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		out.WriteByte('\n')
	}
	out.Write(block)

	// Validate the combined file decodes before it replaces zing.toml, which
	// holds github_token: a shape emptyProjectsArrayPattern does not match (a
	// non-empty inline projects array, say) would otherwise duplicate the
	// projects key and make every later Load fail, stopping zing serve. On a
	// decode failure, leave path untouched.
	var check Config
	if _, decErr := toml.Decode(out.String(), &check); decErr != nil {
		return fmt.Errorf("zing.toml: append project: combined config is invalid, left unchanged: %w", decErr)
	}

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "zing.toml.*.tmp")
	if err != nil {
		return fmt.Errorf("zing.toml: append project: %w", err)
	}
	tmp := f.Name()

	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("zing.toml: append project: %w", err)
	}
	if _, err := f.Write(out.Bytes()); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("zing.toml: append project: %w", err)
	}
	// Flush to disk before the rename so a crash cannot leave a truncated
	// zing.toml that loses user, github_token, and every project.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("zing.toml: append project: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("zing.toml: append project: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("zing.toml: append project: %w", err)
	}
	return nil
}

func checkUnknownKeys(md toml.MetaData) error {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	keys := make([]string, len(undecoded))
	for i, k := range undecoded {
		keys[i] = k.String()
	}
	sort.Strings(keys)
	return fmt.Errorf("zing.toml: unknown key %s", keys[0])
}

// checkRequiredKeys checks the keys required of every config: user,
// github_token, and (unless allowEmptyProjects, LoadForAdd's one relaxation)
// at least one project with its own required per-project keys.
func checkRequiredKeys(cfg Config, allowEmptyProjects bool) error {
	if cfg.User == "" {
		return errors.New("zing.toml: missing required key user")
	}
	if cfg.GitHubToken == "" {
		return errors.New("zing.toml: missing required key github_token")
	}
	if !allowEmptyProjects && len(cfg.Projects) == 0 {
		return errors.New("zing.toml: missing required key projects")
	}

	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		switch {
		case p.Name == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].name", i)
		case p.Repo == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].repo", i)
		case p.Path == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].path", i)
		case p.Tracker == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].tracker", i)
		case p.Commands.Test == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].commands.test", i)
		case p.Commands.Lint == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].commands.lint", i)
		}
	}
	return nil
}

// checkValues runs the fixed-order value checks. A field with a default
// (floor, method, port, usage_hold_percent, agent_minutes_per_ticket) is
// only checked when the key was explicitly present, so an absent key defers
// to applyDefaults and an explicit out-of-range value (including an explicit
// zero) is rejected here.
func checkValues(md toml.MetaData, cfg Config) error {
	if md.IsDefined("review", "floor") && !slices.Contains(validReviewFloors, cfg.Review.Floor) {
		return fmt.Errorf("zing.toml: review.floor: must be one of %s", strings.Join(validReviewFloors, ", "))
	}
	if md.IsDefined("merge", "method") && !slices.Contains(validMergeMethods, cfg.Merge.Method) {
		return fmt.Errorf("zing.toml: merge.method: must be one of %s", strings.Join(validMergeMethods, ", "))
	}
	if md.IsDefined("review", "max_lenses_parallel") &&
		(cfg.Review.MaxLensesParallel < minLensesParallel || cfg.Review.MaxLensesParallel > maxLensesParallel) {
		return fmt.Errorf("zing.toml: review.max_lenses_parallel: must be %d to %d", minLensesParallel, maxLensesParallel)
	}
	// Only when explicit: resolveJudgeCodexHome already built an absolute
	// default (PKG9-PLAN.md section 4.5, D27).
	if md.IsDefined("judge_codex_home") && !filepath.IsAbs(cfg.JudgeCodexHome) {
		return errors.New("zing.toml: judge_codex_home must be an absolute path")
	}
	for i := range cfg.Projects {
		if p := &cfg.Projects[i]; p.Tracker != "github" {
			return fmt.Errorf("zing.toml: projects[%d].tracker: must be github", i)
		}
	}
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		if p.Intake.Mode != "" && !slices.Contains(validIntakeModes, p.Intake.Mode) {
			return fmt.Errorf("zing.toml: project %s: intake.mode must be auto or manual", p.Name)
		}
	}
	if md.IsDefined("console", "port") && (cfg.Console.Port < 1 || cfg.Console.Port > 65535) {
		return errors.New("zing.toml: console.port: must be 1 to 65535")
	}
	if err := checkBindAddresses(cfg.Console.Bind); err != nil {
		return err
	}
	if err := checkAllowedHosts(cfg.Console.AllowedHosts); err != nil {
		return err
	}
	if err := checkSandboxReadPaths(cfg.Sandbox.ReadPaths); err != nil {
		return err
	}
	if err := checkReviewBots(md, cfg.ReviewBots); err != nil {
		return err
	}
	if md.IsDefined("budget", "usage_hold_percent") &&
		(cfg.Budget.UsageHoldPercent < 0 || cfg.Budget.UsageHoldPercent > 100) {
		return errors.New("zing.toml: budget.usage_hold_percent: must be 0 to 100")
	}
	if md.IsDefined("budget", "agent_minutes_per_ticket") &&
		(cfg.Budget.AgentMinutesPerTicket < minBudgetMinutes || cfg.Budget.AgentMinutesPerTicket > maxBudgetMinutes) {
		return fmt.Errorf("zing.toml: budget.agent_minutes_per_ticket: must be between %d and %d minutes", minBudgetMinutes, maxBudgetMinutes)
	}
	if md.IsDefined("console", "push_token") && utf8.RuneCountInString(cfg.Console.PushToken) < minPushTokenLen {
		return fmt.Errorf("zing.toml: console.push_token: must be at least %d characters", minPushTokenLen)
	}
	// Project names must be unique: serve keys the tracker's repo map by name
	// (cmd/zing's productionTracker), so two projects sharing a name would
	// silently collide and send one project's intake and comments to the
	// other's repository.
	seenProjectNames := make(map[string]bool, len(cfg.Projects))
	for i := range cfg.Projects {
		name := cfg.Projects[i].Name
		if seenProjectNames[name] {
			return fmt.Errorf("zing.toml: duplicate project name %q: each project's name must be unique", name)
		}
		seenProjectNames[name] = true
	}
	return nil
}

// checkBindAddresses rejects a wildcard console.bind entry (design section
// 6.14: "netip.Addr.IsUnspecified, that is 0.0.0.0 or ::"), because a
// wildcard listener has no single browser Host authority and the no-login
// console must bind concrete addresses only. A "tailscale" token, or any
// other entry that does not parse as an IP at all, is left for cmd/zing's
// resolver to handle and is not an error here.
//
// It also rejects an empty or whitespace-only entry (security fix, PR #16
// review): cmd/zing/bind.go's resolveBindHosts passes a non-"tailscale"
// token straight through unresolved, and serve.go's listenOnAll then builds
// its listen address with net.JoinHostPort(host, port); JoinHostPort("",
// port) yields ":port", which net.Listen binds to every interface. A blank
// bind entry is never a legitimate value the resolver can act on (unlike
// "tailscale" or a literal IP), so it is rejected here rather than left for
// cmd/zing, the same way a wildcard address is.
//
// It also rejects an entry with leading or trailing whitespace (PR review
// fix), rather than trimming it: a padded literal IP like " 127.0.0.1 "
// used to pass this check (netip.ParseAddr rejects the surrounding
// whitespace, so the entry fell through to the "not an IP, leave it for
// cmd/zing" branch untouched) and then reached bind.go's resolveBindHosts
// unresolved, same as "tailscale" would, but as a literal string neither
// stripped nor recognized; serve.go's listenOnAll then built
// net.JoinHostPort(" 127.0.0.1 ", port) and net.Listen failed at startup,
// long after config.Load had already reported success. Trimming in place
// here would work too -- checkValues, this function's only caller, takes
// its Config by value, but a slice field's backing array is still shared
// with Load's own cfg, so writing the trimmed string back into bind[i]
// would reach cmd/zing without changing it -- but that only works because
// of that aliasing, which is easy to break by accident in a later refactor
// (switching checkValues to take *Config, or checkBindAddresses to copy its
// slice, would silently stop the trim from ever reaching cmd/zing). A clear
// rejection here does not depend on that, so it is the one this function
// makes.
func checkBindAddresses(bind []string) error {
	for i, b := range bind {
		trimmed := strings.TrimSpace(b)
		if trimmed == "" {
			return fmt.Errorf("zing.toml: console.bind[%d]: must not be empty", i)
		}
		if trimmed != b {
			return fmt.Errorf("zing.toml: console.bind[%d]: must not have leading or trailing whitespace", i)
		}
		addr, err := netip.ParseAddr(b)
		if err != nil {
			continue
		}
		if addr.IsUnspecified() {
			return errors.New("zing.toml: console.bind: wildcard address not allowed")
		}
	}
	return nil
}

// checkAllowedHosts rejects a console.allowed_hosts entry that is not a
// bare hostname (design section 6.14: "allowed_hosts entries are hostnames
// without a port, validated at config load"). A hostname never contains a
// colon, so testing for one catches both a well-formed "host:port" entry
// and a malformed authority (cubic review fix, PR #16: net.SplitHostPort's
// error varies by shape -- "host:80" parses clean, but "host:" and
// "h:o:st" each fail differently -- so gating on SplitHostPort's error
// alone let a malformed colon-bearing entry like "h:o:st" slip through
// unrejected; a plain colon check has no such gap).
func checkAllowedHosts(hosts []string) error {
	for i, h := range hosts {
		if strings.Contains(h, ":") {
			return fmt.Errorf("zing.toml: console.allowed_hosts[%d]: must not include a port", i)
		}
	}
	return nil
}

// checkSandboxReadPaths rejects a sandbox.read_paths entry that is not
// absolute, or that carries a '"', '\', or a newline (design section 5.4):
// each entry is substituted straight into the seatbelt profile as an
// "(allow file-read-data (subpath "<path>"))" line, so the same safety rule
// the sandbox package's own params enforce applies here too.
func checkSandboxReadPaths(paths []string) error {
	for i, p := range paths {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\"\\\n") {
			return fmt.Errorf("zing.toml: sandbox.read_paths[%d] must be an absolute path without quotes or backslashes", i)
		}
	}
	return nil
}

// checkReviewBots rejects an explicit review_bots.wait_minutes outside
// [1, 1440], and any review_bots.checks entry with an empty check or
// trigger, or a check name repeated by an earlier entry.
func checkReviewBots(md toml.MetaData, bots ReviewBots) error {
	outOfRange := bots.WaitMinutes < minReviewBotWaitMinutes || bots.WaitMinutes > maxReviewBotWaitMinutes
	if md.IsDefined("review_bots", "wait_minutes") && outOfRange {
		return fmt.Errorf("zing.toml: review_bots.wait_minutes: must be %d to %d", minReviewBotWaitMinutes, maxReviewBotWaitMinutes)
	}
	seen := make(map[string]bool, len(bots.Checks))
	for i := range bots.Checks {
		c := &bots.Checks[i]
		switch {
		case strings.TrimSpace(c.Check) == "":
			return fmt.Errorf("zing.toml: review_bots.checks[%d].check: must not be empty", i)
		case strings.TrimSpace(c.Trigger) == "":
			return fmt.Errorf("zing.toml: review_bots.checks[%d].trigger: must not be empty", i)
		case seen[c.Check]:
			return fmt.Errorf("zing.toml: review_bots.checks[%d].check: duplicate check %q", i, c.Check)
		}
		seen[c.Check] = true
	}
	return nil
}

func applyDefaults(md toml.MetaData, cfg *Config) {
	if !md.IsDefined("console", "bind") {
		cfg.Console.Bind = []string{"127.0.0.1", "tailscale"}
	}
	if !md.IsDefined("console", "port") {
		cfg.Console.Port = 7420
	}
	if !md.IsDefined("models", "sonnet") {
		cfg.Models.Sonnet = "claude-sonnet-5"
	}
	if !md.IsDefined("models", "opus") {
		cfg.Models.Opus = "claude-opus-5-5"
	}
	if !md.IsDefined("models", "fable") {
		cfg.Models.Fable = "claude-fable-5-1"
	}
	if !md.IsDefined("models", "codex") {
		cfg.Models.Codex = "gpt-6-luna"
	}
	if !md.IsDefined("dispatch", "interval_seconds") {
		cfg.Dispatch.IntervalSeconds = 30
	}
	if !md.IsDefined("dispatch", "max_parallel") {
		cfg.Dispatch.MaxParallel = 2
	}
	if !md.IsDefined("budget", "agent_minutes_per_ticket") {
		cfg.Budget.AgentMinutesPerTicket = 240
	}
	if !md.IsDefined("budget", "usage_hold_percent") {
		cfg.Budget.UsageHoldPercent = 80
	}
	if !md.IsDefined("review", "floor") {
		cfg.Review.Floor = "minor"
	}
	if !md.IsDefined("review", "max_lenses_parallel") {
		cfg.Review.MaxLensesParallel = maxLensesParallel
	}
	if !md.IsDefined("merge", "method") {
		cfg.Merge.Method = "squash"
	}
	if !md.IsDefined("merge", "manual_paths") {
		cfg.Merge.ManualPaths = []string{"deploy/**", "**/migrations/**", "Dockerfile", ".github/workflows/**"}
	}
	if !md.IsDefined("merge", "dependency_files") {
		cfg.Merge.DependencyFiles = []string{"go.mod", "go.sum", "package.json", "pyproject.toml"}
	}
	if !md.IsDefined("review_bots", "wait_minutes") {
		cfg.ReviewBots.WaitMinutes = defaultReviewBotWaitMinutes
	}
	if !md.IsDefined("review_bots", "checks") {
		cfg.ReviewBots.Checks = slices.Clone(defaultReviewBotChecks)
	}

	for i := range cfg.Projects {
		if cfg.Projects[i].Intake.Mode == "" {
			cfg.Projects[i].Intake.Mode = IntakeModeAuto
		}
		// assigned_to only defaults to the top-level user in auto mode
		// (PKG9-PLAN.md D29): manual mode's assigned_to is not required and
		// may stay absent, since the dispatcher never calls Tracker.Intake
		// for a manual project.
		if cfg.Projects[i].Intake.Mode == IntakeModeAuto && cfg.Projects[i].Intake.AssignedTo == "" {
			cfg.Projects[i].Intake.AssignedTo = cfg.User
		}
		// DefaultBranch is deliberately left "" when zing.toml omits it,
		// rather than defaulted to "main" here: store.EnsureProject already
		// defaults ""->"main" on INSERT and, on an existing project, skips
		// its reconcile of DefaultBranch entirely when the incoming value is
		// "". A default of "main" applied here would instead reach
		// EnsureProject as an explicit value on every start, and overwrite a
		// real, non-"main" default branch recorded from an earlier "zing
		// project add" with the wrong one.
	}
}
