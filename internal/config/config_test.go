package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const testUser = "peter"

// testGitHubToken is the github_token value every valid fixture below uses,
// so a fixture missing it is unambiguously testing that absence.
const testGitHubToken = "ghp_test_token_0123456789"

// wantWildcardBindError is the exact error checkBindAddresses returns for a
// wildcard console.bind entry (config.go, design section 6.14).
const wantWildcardBindError = "zing.toml: console.bind: wildcard address not allowed"

// wantAllowedHostsPortError is the exact error checkAllowedHosts returns
// for any console.allowed_hosts[0] entry that carries a colon, well-formed
// or malformed alike (config.go, design section 6.14).
const wantAllowedHostsPortError = "zing.toml: console.allowed_hosts[0]: must not include a port"

// writeTOML writes body to a fresh zing.toml under t.TempDir and returns its path.
func writeTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zing.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

const minimalValidTOML = `
user = "peter"
github_token = "ghp_test_token_0123456789"

[[projects]]
name = "zing"
repo = "git@github.com:x/zing.git"
path = "/home/peter/zing"
tracker = "github"

[projects.commands]
test = "go test ./..."
lint = "golangci-lint run"
`

func TestLoad_MinimalConfigGetsEveryDefault(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeTOML(t, minimalValidTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := &Config{
		User:        testUser,
		GitHubToken: testGitHubToken,
		Console: Console{
			Bind: []string{"127.0.0.1", "tailscale"},
			Port: 7420,
			// PushToken defaults to empty: cmd/zing/serve.go resolves and
			// persists the effective token (design section 6.13), not Load.
			// AllowedHosts defaults to empty too.
		},
		Models: Models{
			Sonnet: "claude-sonnet-5",
			Opus:   "claude-opus-4-8",
			Fable:  "claude-fable-5-1",
			Codex:  "gpt-5.5",
		},
		Dispatch: Dispatch{IntervalSeconds: 30, MaxParallel: 2},
		Budget:   Budget{AgentMinutesPerTicket: 240, UsageHoldPercent: 80},
		Review:   Review{Floor: "minor"},
		Merge: Merge{
			Auto:            false,
			Method:          "squash",
			ManualPaths:     []string{"deploy/**", "**/migrations/**", "Dockerfile", ".github/workflows/**"},
			DependencyFiles: []string{"go.mod", "go.sum", "package.json", "pyproject.toml"},
		},
		Projects: []Project{
			{
				Name: "zing", Repo: "git@github.com:x/zing.git", Path: "/home/peter/zing", Tracker: "github",
				// DefaultBranch stays "" when zing.toml omits it (PR review
				// finding Q): store.EnsureProject, not config.Load, is what
				// defaults an absent value to "main", and only on INSERT,
				// so a defaulted "main" here could never overwrite a real
				// default branch recorded by an earlier "zing project add".
				DefaultBranch: "",
				Self:          false,
				Intake:        Intake{AssignedTo: testUser}, // defaults to the top-level user
				Commands:      Commands{Test: "go test ./...", Lint: "golangci-lint run"},
			},
		},
	}

	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoad_FullConfigKeepsExplicitValues(t *testing.T) {
	t.Parallel()

	const full = `
user = "peter"
github_token = "ghp_test_token_0123456789"

[console]
bind = ["127.0.0.1"]
port = 8080
push_token = "explicit-token-16+"
allowed_hosts = ["example.tailnet", "another.example"]

[models]
sonnet = "custom-sonnet"
opus = "custom-opus"
fable = "custom-fable"
codex = "custom-codex"

[dispatch]
interval_seconds = 60
max_parallel = 4

[budget]
agent_minutes_per_ticket = 120
usage_hold_percent = 50

[review]
floor = "blocker"

[merge]
auto = true
method = "merge"
manual_paths = ["a/**"]
dependency_files = ["go.mod"]

[[projects]]
name = "zing"
repo = "git@github.com:x/zing.git"
path = "/home/peter/zing"
tracker = "github"
default_branch = "develop"
self = true

[projects.intake]
assigned_to = "someone-else"

[projects.commands]
test = "go test ./..."
lint = "golangci-lint run"
`
	cfg, err := Load(writeTOML(t, full))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := &Config{
		User:        testUser,
		GitHubToken: testGitHubToken,
		Console: Console{
			Bind:         []string{"127.0.0.1"},
			Port:         8080,
			PushToken:    "explicit-token-16+",
			AllowedHosts: []string{"example.tailnet", "another.example"},
		},
		Models: Models{
			Sonnet: "custom-sonnet",
			Opus:   "custom-opus",
			Fable:  "custom-fable",
			Codex:  "custom-codex",
		},
		Dispatch: Dispatch{IntervalSeconds: 60, MaxParallel: 4},
		Budget:   Budget{AgentMinutesPerTicket: 120, UsageHoldPercent: 50},
		Review:   Review{Floor: "blocker"},
		Merge: Merge{
			Auto:            true,
			Method:          "merge",
			ManualPaths:     []string{"a/**"},
			DependencyFiles: []string{"go.mod"},
		},
		Projects: []Project{
			{
				Name: "zing", Repo: "git@github.com:x/zing.git", Path: "/home/peter/zing", Tracker: "github",
				DefaultBranch: "develop",
				Self:          true,
				Intake:        Intake{AssignedTo: "someone-else"},
				Commands:      Commands{Test: "go test ./...", Lint: "golangci-lint run"},
			},
		},
	}

	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

// TestLoad_AllowedHostsParsesAndDefaultsEmpty proves design section 6.14's
// console.allowed_hosts is nil (empty) by default and parses to a plain
// string slice when set.
func TestLoad_AllowedHostsParsesAndDefaultsEmpty(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeTOML(t, minimalValidTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Console.AllowedHosts) != 0 {
		t.Errorf("Console.AllowedHosts = %v, want empty by default", cfg.Console.AllowedHosts)
	}

	const withHosts = minimalValidTOML + "\n[console]\nallowed_hosts = [\"example.tailnet\", \"box.local\"]\n"
	cfg, err = Load(writeTOML(t, withHosts))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"example.tailnet", "box.local"}
	if !reflect.DeepEqual(cfg.Console.AllowedHosts, want) {
		t.Errorf("Console.AllowedHosts = %v, want %v", cfg.Console.AllowedHosts, want)
	}
}

func TestLoad_IntakeAssignedToDefaultsWhenExplicitlyEmpty(t *testing.T) {
	t.Parallel()

	const body = `
user = "peter"
github_token = "ghp_test_token_0123456789"

[[projects]]
name = "zing"
repo = "git@github.com:x/zing.git"
path = "/home/peter/zing"
tracker = "github"

[projects.intake]
assigned_to = ""

[projects.commands]
test = "go test ./..."
lint = "golangci-lint run"
`
	cfg, err := Load(writeTOML(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Projects[0].Intake.AssignedTo != testUser {
		t.Errorf("Intake.AssignedTo = %q, want peter (top-level user)", cfg.Projects[0].Intake.AssignedTo)
	}
}

func TestLoad_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			// Placed before any [table] header, so it decodes as a
			// root-level unknown key rather than landing inside whichever
			// table happens to be open last in the document.
			name: "unknown key",
			body: "bogus = \"x\"\n" + minimalValidTOML,
			want: "zing.toml: unknown key bogus",
		},
		{
			name: "multiple unknown keys reports the alphabetically first",
			body: "zzz = 1\naaa = 2\n" + minimalValidTOML,
			want: "zing.toml: unknown key aaa",
		},
		{
			name: "missing user",
			body: `
[[projects]]
name = "zing"
repo = "git@github.com:x/zing.git"
path = "/home/peter/zing"
tracker = "github"

[projects.commands]
test = "go test ./..."
lint = "golangci-lint run"
`,
			want: "zing.toml: missing required key user",
		},
		{
			name: "missing projects",
			body: "user = \"peter\"\ngithub_token = \"" + testGitHubToken + "\"\n",
			want: "zing.toml: missing required key projects",
		},
		{
			name: "missing projects[0].name",
			body: `
user = "peter"
github_token = "` + testGitHubToken + `"

[[projects]]
repo = "git@github.com:x/zing.git"
path = "/home/peter/zing"
tracker = "github"

[projects.commands]
test = "go test ./..."
lint = "golangci-lint run"
`,
			want: "zing.toml: missing required key projects[0].name",
		},
		{
			name: "missing projects[0].commands.test",
			body: `
user = "peter"
github_token = "` + testGitHubToken + `"

[[projects]]
name = "zing"
repo = "git@github.com:x/zing.git"
path = "/home/peter/zing"
tracker = "github"

[projects.commands]
lint = "golangci-lint run"
`,
			want: "zing.toml: missing required key projects[0].commands.test",
		},
		{
			name: "bad review.floor",
			body: minimalValidTOML + "\n[review]\nfloor = \"urgent\"\n",
			want: "zing.toml: review.floor: must be one of blocker, major, minor, nit",
		},
		{
			name: "bad merge.method",
			body: minimalValidTOML + "\n[merge]\nmethod = \"octopus\"\n",
			want: "zing.toml: merge.method: must be one of squash, merge, rebase",
		},
		{
			name: "bad project tracker",
			body: `
user = "peter"
github_token = "` + testGitHubToken + `"

[[projects]]
name = "zing"
repo = "git@github.com:x/zing.git"
path = "/home/peter/zing"
tracker = "gitlab"

[projects.commands]
test = "go test ./..."
lint = "golangci-lint run"
`,
			want: "zing.toml: projects[0].tracker: must be github",
		},
		{
			name: "bad console.port too high",
			body: minimalValidTOML + "\n[console]\nport = 99999\n",
			want: "zing.toml: console.port: must be 1 to 65535",
		},
		{
			name: "explicit console.port = 0 is rejected, not defaulted",
			body: minimalValidTOML + "\n[console]\nport = 0\n",
			want: "zing.toml: console.port: must be 1 to 65535",
		},
		{
			name: "bad budget.usage_hold_percent",
			body: minimalValidTOML + "\n[budget]\nusage_hold_percent = 150\n",
			want: "zing.toml: budget.usage_hold_percent: must be 0 to 100",
		},
		{
			name: "wildcard bind IPv4",
			body: minimalValidTOML + "\n[console]\nbind = [\"0.0.0.0\"]\n",
			want: wantWildcardBindError,
		},
		{
			name: "wildcard bind IPv6",
			body: minimalValidTOML + "\n[console]\nbind = [\"::\"]\n",
			want: wantWildcardBindError,
		},
		{
			name: "wildcard bind among other entries",
			body: minimalValidTOML + "\n[console]\nbind = [\"127.0.0.1\", \"0.0.0.0\"]\n",
			want: wantWildcardBindError,
		},
		{
			name: "allowed_hosts entry with a port",
			body: minimalValidTOML + "\n[console]\nallowed_hosts = [\"example.tailnet:7420\"]\n",
			want: wantAllowedHostsPortError,
		},
		{
			// PR #16 review: net.SplitHostPort("h:o:st") itself errors ("too
			// many colons in address"), so gating the old check on
			// SplitHostPort's error alone let this malformed entry through
			// unrejected.
			name: "allowed_hosts entry malformed with multiple colons",
			body: minimalValidTOML + "\n[console]\nallowed_hosts = [\"h:o:st\"]\n",
			want: wantAllowedHostsPortError,
		},
		{
			// PR #16 review: net.SplitHostPort("host:") succeeds with an
			// empty port, so the old check already caught this one; kept as
			// a regression case alongside the multi-colon one above.
			name: "allowed_hosts entry with a trailing colon and no port",
			body: minimalValidTOML + "\n[console]\nallowed_hosts = [\"host:\"]\n",
			want: wantAllowedHostsPortError,
		},
		{
			// SECURITY, PR #16 review: an empty bind entry parses as neither
			// a wildcard IP nor "tailscale", so cmd/zing/bind.go passed it
			// through unresolved and net.JoinHostPort("", port) bound every
			// interface.
			name: "empty bind entry",
			body: minimalValidTOML + "\n[console]\nbind = [\"\"]\n",
			want: "zing.toml: console.bind[0]: must not be empty",
		},
		{
			name: "whitespace-only bind entry",
			body: minimalValidTOML + "\n[console]\nbind = [\"127.0.0.1\", \"   \"]\n",
			want: "zing.toml: console.bind[1]: must not be empty",
		},
		{
			// PR review: " 127.0.0.1 " parsed as neither a wildcard IP
			// (netip.ParseAddr rejects the surrounding whitespace) nor
			// "tailscale", so it used to pass this check and reach
			// cmd/zing/bind.go unresolved, then fail net.Listen at startup
			// with a malformed host, long after Load had already reported
			// success.
			name: "bind entry with leading and trailing whitespace",
			body: minimalValidTOML + "\n[console]\nbind = [\" 127.0.0.1 \"]\n",
			want: "zing.toml: console.bind[0]: must not have leading or trailing whitespace",
		},
		{
			name: "bind entry with only trailing whitespace",
			body: minimalValidTOML + "\n[console]\nbind = [\"tailscale \"]\n",
			want: "zing.toml: console.bind[0]: must not have leading or trailing whitespace",
		},
		{
			name: "explicit push_token shorter than the minimum",
			body: minimalValidTOML + "\n[console]\npush_token = \"short\"\n",
			want: "zing.toml: console.push_token: must be at least 16 characters",
		},
		{
			name: "explicit push_token empty string is still rejected, not left to the auto-generated default",
			body: minimalValidTOML + "\n[console]\npush_token = \"\"\n",
			want: "zing.toml: console.push_token: must be at least 16 characters",
		},
		{
			name: "missing github_token",
			body: `
user = "peter"

[[projects]]
name = "zing"
repo = "git@github.com:x/zing.git"
path = "/home/peter/zing"
tracker = "github"

[projects.commands]
test = "go test ./..."
lint = "golangci-lint run"
`,
			want: "zing.toml: missing required key github_token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(writeTOML(t, tt.body))
			if err == nil {
				t.Fatalf("Load() = nil, want error %q", tt.want)
			}
			if err.Error() != tt.want {
				t.Errorf("Load() = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

// TestLoad_PushTokenCountsRunesNotBytes proves minPushTokenLen's floor is
// counted in runes (config.go's utf8.RuneCountInString), matching the error
// message's "16 characters" wording: a token built from eight 4-byte emoji
// is 32 bytes long but only 8 runes, short of the 16-character floor, and
// must be rejected even though a byte-length check would have wrongly
// accepted it.
func TestLoad_PushTokenCountsRunesNotBytes(t *testing.T) {
	t.Parallel()

	const eightEmoji = "😀😀😀😀😀😀😀😀" // 8 runes, 32 bytes
	_, err := Load(writeTOML(t, minimalValidTOML+"\n[console]\npush_token = \""+eightEmoji+"\"\n"))
	if err == nil {
		t.Fatal("Load() = nil, want an error for an 8-rune (32-byte) push_token")
	}
	const want = "zing.toml: console.push_token: must be at least 16 characters"
	if err.Error() != want {
		t.Errorf("Load() = %q, want %q", err.Error(), want)
	}

	// A genuinely 16-rune multi-byte token clears the same floor.
	const sixteenEmoji = eightEmoji + eightEmoji // 16 runes, 64 bytes
	cfg, err := Load(writeTOML(t, minimalValidTOML+"\n[console]\npush_token = \""+sixteenEmoji+"\"\n"))
	if err != nil {
		t.Fatalf("Load() with a 16-rune push_token: %v", err)
	}
	if cfg.Console.PushToken != sixteenEmoji {
		t.Errorf("Console.PushToken = %q, want %q", cfg.Console.PushToken, sixteenEmoji)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Parallel()

	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if filepath.Base(path) != "zing.toml" {
		t.Errorf("DefaultPath() = %q, want a path ending in zing.toml", path)
	}
	if filepath.Base(filepath.Dir(path)) != ".zing" {
		t.Errorf("DefaultPath() = %q, want the parent directory to be .zing", path)
	}
}

// TestSave_RoundTripsThroughLoad proves Save's write half and Load's read
// half agree: a config loaded, saved to a fresh path (whose parent directory
// does not exist yet, exercising Save's MkdirAll), then loaded again comes
// back identical, including PushToken's "omitempty" tag (config.go) not
// resurfacing as an explicit empty string that checkValues would then
// reject.
func TestSave_RoundTripsThroughLoad(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeTOML(t, minimalValidTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "nested", "zing.toml")
	if saveErr := Save(dst, cfg); saveErr != nil {
		t.Fatalf("Save: %v", saveErr)
	}

	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load(Save(cfg)): %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("round-tripped config = %+v, want %+v", got, cfg)
	}
}

// TestSave_WritesFileAt0600 proves Save's file lands at 0600, since it
// carries github_token (PKG5-PLAN.md section 9).
func TestSave_WritesFileAt0600(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeTOML(t, minimalValidTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	path := filepath.Join(t.TempDir(), "zing.toml")
	if saveErr := Save(path, cfg); saveErr != nil {
		t.Fatalf("Save: %v", saveErr)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("saved file mode = %o, want 0600", perm)
	}
}

// leftoverSaveTempFiles lists the "zing.toml.*.tmp" entries in dir, the
// pattern Save's os.CreateTemp call uses, so a test can prove none is left
// behind after a failed or successful Save.
func leftoverSaveTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var leftover []string
	for _, e := range entries {
		matched, matchErr := filepath.Match("zing.toml.*.tmp", e.Name())
		if matchErr != nil {
			t.Fatalf("Match: %v", matchErr)
		}
		if matched {
			leftover = append(leftover, e.Name())
		}
	}
	return leftover
}

// TestSave_ErrorLeavesTempRemovedAndOriginalUntouched proves the edge case
// in PKG5-PLAN.md section 13 ("config.Save cannot write"): a rename failure
// (forced here by making the destination an existing directory) removes the
// sibling temp file and leaves whatever was already at path untouched,
// since the rename that would have replaced it never completed.
func TestSave_ErrorLeavesTempRemovedAndOriginalUntouched(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeTOML(t, minimalValidTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "zing.toml")
	if mkdirErr := os.Mkdir(path, 0o700); mkdirErr != nil {
		t.Fatalf("Mkdir: %v", mkdirErr)
	}

	if saveErr := Save(path, cfg); saveErr == nil {
		t.Fatal("Save() = nil, want an error when the rename target is an existing directory")
	}

	if leftover := leftoverSaveTempFiles(t, dir); len(leftover) != 0 {
		t.Errorf("temp files left behind after a failed Save: %v", leftover)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(path) after a failed Save: %v", err)
	}
	if !info.IsDir() {
		t.Error("path was overwritten by a failed Save, want the original left untouched")
	}
}

// TestSave_PreexistingDotTmpFileDoesNotBreakSave proves PR review finding I:
// Save's old fixed "<path>.tmp" name opened with O_EXCL meant a single
// leftover temp file -- from an earlier crashed or interrupted Save --
// would permanently block every Save after it. Save now writes to a fresh,
// uniquely named file from os.CreateTemp, so a stale "<path>.tmp" sitting
// alongside it is just another file in the directory, not an obstacle.
func TestSave_PreexistingDotTmpFileDoesNotBreakSave(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeTOML(t, minimalValidTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "zing.toml")
	stalePath := path + ".tmp"
	if writeErr := os.WriteFile(stalePath, []byte("stale, from a crashed Save"), 0o600); writeErr != nil {
		t.Fatalf("write stale %s: %v", stalePath, writeErr)
	}

	if saveErr := Save(path, cfg); saveErr != nil {
		t.Fatalf("Save: unexpected error with a pre-existing %s: %v", stalePath, saveErr)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load(Save(cfg)): %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("round-tripped config = %+v, want %+v", got, cfg)
	}

	if _, statErr := os.Stat(stalePath); statErr != nil {
		t.Errorf("expected the unrelated stale %s to be left alone, stat err = %v", stalePath, statErr)
	}
}

// TestLoad_RepairsGroupReadableTokenFileTo0600 proves a token-bearing
// zing.toml left group- or other-readable (0644 here) is chmod'd to 0600 as
// a side effect of Load, before the token is ever read out of it.
func TestLoad_RepairsGroupReadableTokenFileTo0600(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, minimalValidTOML)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode after Load = %o, want repaired to 0600", perm)
	}
}

// TestLoadForAdd_AcceptsZeroProjects proves LoadForAdd's one relaxation from
// Load (PKG5-PLAN.md section 9): a config with user and github_token but no
// [[projects]] loads clean, so "zing project add" can bootstrap the first
// project.
func TestLoadForAdd_AcceptsZeroProjects(t *testing.T) {
	t.Parallel()

	body := "user = \"peter\"\ngithub_token = \"" + testGitHubToken + "\"\n"
	cfg, err := LoadForAdd(writeTOML(t, body))
	if err != nil {
		t.Fatalf("LoadForAdd: %v", err)
	}
	if len(cfg.Projects) != 0 {
		t.Errorf("Projects = %v, want empty", cfg.Projects)
	}
	if cfg.GitHubToken != testGitHubToken {
		t.Errorf("GitHubToken = %q, want %q", cfg.GitHubToken, testGitHubToken)
	}
}

// TestLoadForAdd_StillRequiresGitHubToken proves LoadForAdd relaxes only the
// zero-projects requirement, not github_token.
func TestLoadForAdd_StillRequiresGitHubToken(t *testing.T) {
	t.Parallel()

	_, err := LoadForAdd(writeTOML(t, `user = "peter"`))
	if err == nil {
		t.Fatal("LoadForAdd() = nil, want an error for missing github_token")
	}
	const want = "zing.toml: missing required key github_token"
	if err.Error() != want {
		t.Errorf("LoadForAdd() = %q, want %q", err.Error(), want)
	}
}

// TestLoad_RejectsZeroProjects proves Load, unlike LoadForAdd, still
// requires at least one project.
func TestLoad_RejectsZeroProjects(t *testing.T) {
	t.Parallel()

	body := "user = \"peter\"\ngithub_token = \"" + testGitHubToken + "\"\n"
	_, err := Load(writeTOML(t, body))
	if err == nil {
		t.Fatal("Load() = nil, want an error for zero projects")
	}
	const want = "zing.toml: missing required key projects"
	if err.Error() != want {
		t.Errorf("Load() = %q, want %q", err.Error(), want)
	}
}
