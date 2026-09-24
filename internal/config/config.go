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
	GitHubToken string    `toml:"github_token"`
	Console     Console   `toml:"console"`
	Models      Models    `toml:"models"`
	Dispatch    Dispatch  `toml:"dispatch"`
	Budget      Budget    `toml:"budget"`
	Review      Review    `toml:"review"`
	Merge       Merge     `toml:"merge"`
	Projects    []Project `toml:"projects"`
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
}

type Merge struct {
	Auto            bool     `toml:"auto"`
	Method          string   `toml:"method"`
	ManualPaths     []string `toml:"manual_paths"`
	DependencyFiles []string `toml:"dependency_files"`
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
}

type Commands struct {
	Test string `toml:"test"`
	Lint string `toml:"lint"`
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
	if err := checkValues(md, cfg); err != nil {
		return nil, err
	}

	applyDefaults(md, &cfg)

	return &cfg, nil
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
// (floor, method, port, usage_hold_percent) is only checked when the key was
// explicitly present, so an absent key defers to applyDefaults and an
// explicit out-of-range value (including an explicit zero) is rejected here.
func checkValues(md toml.MetaData, cfg Config) error {
	if md.IsDefined("review", "floor") && !slices.Contains(validReviewFloors, cfg.Review.Floor) {
		return fmt.Errorf("zing.toml: review.floor: must be one of %s", strings.Join(validReviewFloors, ", "))
	}
	if md.IsDefined("merge", "method") && !slices.Contains(validMergeMethods, cfg.Merge.Method) {
		return fmt.Errorf("zing.toml: merge.method: must be one of %s", strings.Join(validMergeMethods, ", "))
	}
	for i := range cfg.Projects {
		if p := &cfg.Projects[i]; p.Tracker != "github" {
			return fmt.Errorf("zing.toml: projects[%d].tracker: must be github", i)
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
	if md.IsDefined("budget", "usage_hold_percent") &&
		(cfg.Budget.UsageHoldPercent < 0 || cfg.Budget.UsageHoldPercent > 100) {
		return errors.New("zing.toml: budget.usage_hold_percent: must be 0 to 100")
	}
	if md.IsDefined("console", "push_token") && utf8.RuneCountInString(cfg.Console.PushToken) < minPushTokenLen {
		return fmt.Errorf("zing.toml: console.push_token: must be at least %d characters", minPushTokenLen)
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
		cfg.Models.Opus = "claude-opus-4-8"
	}
	if !md.IsDefined("models", "fable") {
		cfg.Models.Fable = "claude-fable-5-1"
	}
	if !md.IsDefined("models", "codex") {
		cfg.Models.Codex = "gpt-5.5"
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
	if !md.IsDefined("merge", "method") {
		cfg.Merge.Method = "squash"
	}
	if !md.IsDefined("merge", "manual_paths") {
		cfg.Merge.ManualPaths = []string{"deploy/**", "**/migrations/**", "Dockerfile", ".github/workflows/**"}
	}
	if !md.IsDefined("merge", "dependency_files") {
		cfg.Merge.DependencyFiles = []string{"go.mod", "go.sum", "package.json", "pyproject.toml"}
	}

	for i := range cfg.Projects {
		if cfg.Projects[i].Intake.AssignedTo == "" {
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
