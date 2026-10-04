package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const testUser = "peter"

// defaultTestJudgeCodexHome returns judge_codex_home's own default,
// expanded against this test process's real home directory (PKG9-PLAN.md
// section 4.5, D27): every Load test whose zing.toml omits the key wants
// this value back.
func defaultTestJudgeCodexHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	return filepath.Join(home, ".zing", "codex-judge")
}

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

// wantBudgetMinutesError is the exact error checkValues returns for any
// budget.agent_minutes_per_ticket value outside [1, 525600] (config.go,
// design section 4.4).
const wantBudgetMinutesError = "zing.toml: budget.agent_minutes_per_ticket: must be between 1 and 525600 minutes"

// testTracker, testCommandTest, and testCommandLint are the tracker and
// commands values every valid Project fixture below uses, pulled out as
// constants so goconst does not flag the repeats.
const (
	testTracker     = "github"
	testCommandTest = "go test ./..."
	testCommandLint = "golangci-lint run"
)

// testZingProjectName is the project name minimalValidTOML (and every TOML
// fixture built from it) uses, pulled out as a constant so goconst does not
// flag the repeats.
const testZingProjectName = "zing"

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
		User:           testUser,
		GitHubToken:    testGitHubToken,
		JudgeCodexHome: defaultTestJudgeCodexHome(t),
		Console: Console{
			Bind: []string{"127.0.0.1", "tailscale"},
			Port: 7420,
			// PushToken defaults to empty: cmd/zing/serve.go resolves and
			// persists the effective token (design section 6.13), not Load.
			// AllowedHosts defaults to empty too.
		},
		Models: Models{
			Sonnet: "claude-sonnet-5",
			Opus:   "claude-opus-5-5",
			Fable:  "claude-fable-5-1",
			Codex:  "gpt-6-luna",
		},
		Dispatch: Dispatch{IntervalSeconds: 30, MaxParallel: 2},
		Budget:   Budget{AgentMinutesPerTicket: 240, UsageHoldPercent: 80},
		Review:   Review{Floor: "minor", MaxLensesParallel: 7},
		Merge: Merge{
			Auto:            false,
			Method:          "squash",
			ManualPaths:     []string{"deploy/**", "**/migrations/**", "Dockerfile", ".github/workflows/**"},
			DependencyFiles: []string{"go.mod", "go.sum", "package.json", "pyproject.toml"},
		},
		Projects: []Project{
			{
				Name: testZingProjectName, Repo: "git@github.com:x/zing.git", Path: "/home/peter/zing", Tracker: testTracker,
				// DefaultBranch stays "" when zing.toml omits it (PR review
				// finding Q): store.EnsureProject, not config.Load, is what
				// defaults an absent value to "main", and only on INSERT,
				// so a defaulted "main" here could never overwrite a real
				// default branch recorded by an earlier "zing project add".
				DefaultBranch: "",
				Self:          false,
				Intake:        Intake{AssignedTo: testUser, Mode: IntakeModeAuto}, // defaults to the top-level user, mode auto
				Commands:      Commands{Test: testCommandTest, Lint: testCommandLint},
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
		User:           testUser,
		GitHubToken:    testGitHubToken,
		JudgeCodexHome: defaultTestJudgeCodexHome(t),
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
		Review:   Review{Floor: "blocker", MaxLensesParallel: 7},
		Merge: Merge{
			Auto:            true,
			Method:          "merge",
			ManualPaths:     []string{"a/**"},
			DependencyFiles: []string{"go.mod"},
		},
		Projects: []Project{
			{
				Name: testZingProjectName, Repo: "git@github.com:x/zing.git", Path: "/home/peter/zing", Tracker: testTracker,
				DefaultBranch: "develop",
				Self:          true,
				Intake:        Intake{AssignedTo: "someone-else", Mode: IntakeModeAuto},
				Commands:      Commands{Test: testCommandTest, Lint: testCommandLint},
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

// TestSandboxReadPaths proves the sandbox.read_paths key (design section
// 5.4): absent defaults empty, a valid absolute path list round-trips, and
// a relative or quote-carrying entry is rejected with the exact error text.
func TestSandboxReadPaths(t *testing.T) {
	t.Parallel()

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		cfg, err := Load(writeTOML(t, minimalValidTOML))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Sandbox.ReadPaths) != 0 {
			t.Errorf("Sandbox.ReadPaths = %v, want empty by default", cfg.Sandbox.ReadPaths)
		}
	})

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		const body = minimalValidTOML + "\n[sandbox]\nread_paths = [\"/opt/homebrew/bin\", \"/Users/peter/.local/share/mise\"]\n"
		cfg, err := Load(writeTOML(t, body))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := []string{"/opt/homebrew/bin", "/Users/peter/.local/share/mise"}
		if diff := cmp.Diff(want, cfg.Sandbox.ReadPaths); diff != "" {
			t.Errorf("Sandbox.ReadPaths mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("relative", func(t *testing.T) {
		t.Parallel()
		const body = minimalValidTOML + "\n[sandbox]\nread_paths = [\"relative/path\"]\n"
		_, err := Load(writeTOML(t, body))
		if err == nil {
			t.Fatal("Load with a relative read_paths entry: want an error, got nil")
		}
		want := "zing.toml: sandbox.read_paths[0] must be an absolute path without quotes or backslashes"
		if err.Error() != want {
			t.Errorf("Load() = %q, want %q", err.Error(), want)
		}
	})

	t.Run("quoted", func(t *testing.T) {
		t.Parallel()
		const body = minimalValidTOML + `
[sandbox]
read_paths = ["/opt/bad\"path"]
`
		_, err := Load(writeTOML(t, body))
		if err == nil {
			t.Fatal("Load with a quote-carrying read_paths entry: want an error, got nil")
		}
		want := "zing.toml: sandbox.read_paths[0] must be an absolute path without quotes or backslashes"
		if err.Error() != want {
			t.Errorf("Load() = %q, want %q", err.Error(), want)
		}
	})
}

// TestClaudeOAuthTokenParsed proves claude_oauth_token (design section 4.5,
// D26) round-trips: absent parses empty (config.Load cannot require it, it
// never reads machine.toml), set parses back unchanged.
func TestClaudeOAuthTokenParsed(t *testing.T) {
	t.Parallel()

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		cfg, err := Load(writeTOML(t, minimalValidTOML))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ClaudeOAuthToken != "" {
			t.Errorf("ClaudeOAuthToken = %q, want empty", cfg.ClaudeOAuthToken)
		}
	})

	t.Run("set", func(t *testing.T) {
		t.Parallel()
		const token = "sk-ant-oat01-test-token"
		// Prepended, not appended: minimalValidTOML ends inside
		// [[projects]]/[projects.commands], so appending a bare key would
		// parse as nesting inside that table instead of at the top level.
		body := "claude_oauth_token = \"" + token + "\"\n" + minimalValidTOML
		cfg, err := Load(writeTOML(t, body))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ClaudeOAuthToken != token {
			t.Errorf("ClaudeOAuthToken = %q, want %q", cfg.ClaudeOAuthToken, token)
		}
	})
}

// TestClaudeOAuthTokenNeverLogged proves Load never writes the
// claude_oauth_token value into a log record, even incidentally: the fixture
// file here is deliberately left group-readable, so repairFileMode's own
// "zing.toml is readable by group or other" warning fires, and that warning
// (the one thing Load logs on this path) is what this test inspects.
func TestClaudeOAuthTokenNeverLogged(t *testing.T) {
	const token = "sk-ant-oat01-do-not-log-this-token"
	// Prepended, not appended: see TestClaudeOAuthTokenParsed's own comment.
	path := writeTOML(t, "claude_oauth_token = \""+token+"\"\n"+minimalValidTOML)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClaudeOAuthToken != token {
		t.Fatalf("ClaudeOAuthToken = %q, want %q", cfg.ClaudeOAuthToken, token)
	}
	if strings.Contains(buf.String(), token) {
		t.Errorf("log output contains the claude_oauth_token value: %s", buf.String())
	}
}

// TestReviewMaxLensesParallel proves review.max_lenses_parallel (design
// section 4.5): absent defaults to 7 (the lens count), 1 and 7 both load,
// and 0 or 8 are refused with the exact error text.
func TestReviewMaxLensesParallel(t *testing.T) {
	t.Parallel()

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		cfg, err := Load(writeTOML(t, minimalValidTOML))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Review.MaxLensesParallel != 7 {
			t.Errorf("MaxLensesParallel = %d, want 7", cfg.Review.MaxLensesParallel)
		}
	})

	for _, n := range []int{1, 7} {
		t.Run(fmt.Sprintf("valid %d", n), func(t *testing.T) {
			t.Parallel()
			body := minimalValidTOML + fmt.Sprintf("\n[review]\nmax_lenses_parallel = %d\n", n)
			cfg, err := Load(writeTOML(t, body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Review.MaxLensesParallel != n {
				t.Errorf("MaxLensesParallel = %d, want %d", cfg.Review.MaxLensesParallel, n)
			}
		})
	}

	for _, n := range []int{0, 8} {
		t.Run(fmt.Sprintf("invalid %d", n), func(t *testing.T) {
			t.Parallel()
			body := minimalValidTOML + fmt.Sprintf("\n[review]\nmax_lenses_parallel = %d\n", n)
			_, err := Load(writeTOML(t, body))
			if err == nil {
				t.Fatal("Load: want an error, got nil")
			}
			want := "zing.toml: review.max_lenses_parallel: must be 1 to 7"
			if err.Error() != want {
				t.Errorf("Load() = %q, want %q", err.Error(), want)
			}
		})
	}
}

// TestJudgeCodexHomeDefault proves judge_codex_home's own default
// (PKG9-PLAN.md section 4.5, D27): absent from zing.toml, it resolves to
// "~/.zing/codex-judge", expanded against this process's real home
// directory.
func TestJudgeCodexHomeDefault(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeTOML(t, minimalValidTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := defaultTestJudgeCodexHome(t); cfg.JudgeCodexHome != want {
		t.Errorf("JudgeCodexHome = %q, want %q", cfg.JudgeCodexHome, want)
	}
}

// TestJudgeCodexHomeExpandsTilde proves an explicit "~/..." value expands
// against the real home directory, the same as the default (section 4.5:
// "~ expanded").
func TestJudgeCodexHomeExpandsTilde(t *testing.T) {
	t.Parallel()

	body := "judge_codex_home = \"~/custom-codex-judge\"\n" + minimalValidTOML
	cfg, err := Load(writeTOML(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	want := filepath.Join(home, "custom-codex-judge")
	if cfg.JudgeCodexHome != want {
		t.Errorf("JudgeCodexHome = %q, want %q", cfg.JudgeCodexHome, want)
	}
}

// TestJudgeCodexHomeExplicitAbsolute proves an explicit, already-absolute
// value loads unchanged.
func TestJudgeCodexHomeExplicitAbsolute(t *testing.T) {
	t.Parallel()

	const explicit = "/opt/zing/codex-judge"
	body := fmt.Sprintf("judge_codex_home = %q\n", explicit) + minimalValidTOML
	cfg, err := Load(writeTOML(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JudgeCodexHome != explicit {
		t.Errorf("JudgeCodexHome = %q, want %q", cfg.JudgeCodexHome, explicit)
	}
}

// TestJudgeCodexHomeMustBeAbsolute proves an explicit relative value (after
// expansion -- no leading "~", so expandHome leaves it unchanged) is
// refused with the exact error text (section 4.5).
func TestJudgeCodexHomeMustBeAbsolute(t *testing.T) {
	t.Parallel()

	body := "judge_codex_home = \"relative/codex-judge\"\n" + minimalValidTOML
	_, err := Load(writeTOML(t, body))
	if err == nil {
		t.Fatal("Load: want an error, got nil")
	}
	want := "zing.toml: judge_codex_home must be an absolute path"
	if err.Error() != want {
		t.Errorf("Load() = %q, want %q", err.Error(), want)
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

// TestLoad_IntakeModeDefaultsToAuto proves an absent intake.mode defaults to
// "auto" (PKG9-PLAN.md D29), and that auto mode keeps today's
// assigned_to-defaults-to-user rule.
func TestLoad_IntakeModeDefaultsToAuto(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeTOML(t, minimalValidTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Projects[0].Intake.Mode != IntakeModeAuto {
		t.Errorf("Intake.Mode = %q, want %q", cfg.Projects[0].Intake.Mode, IntakeModeAuto)
	}
	if cfg.Projects[0].Intake.AssignedTo != testUser {
		t.Errorf("Intake.AssignedTo = %q, want %q (auto mode defaults it to the top-level user)", cfg.Projects[0].Intake.AssignedTo, testUser)
	}
}

// TestLoad_IntakeModeManualLeavesAssignedToEmpty proves manual mode does not
// apply the assigned_to-defaults-to-user rule (PKG9-PLAN.md D29: "assigned_to
// is not required (it may be absent)" in manual mode): an explicit manual
// project with no assigned_to loads with AssignedTo left empty, not
// defaulted to the top-level user.
func TestLoad_IntakeModeManualLeavesAssignedToEmpty(t *testing.T) {
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
mode = "manual"

[projects.commands]
test = "go test ./..."
lint = "golangci-lint run"
`
	cfg, err := Load(writeTOML(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Projects[0].Intake.Mode != IntakeModeManual {
		t.Errorf("Intake.Mode = %q, want %q", cfg.Projects[0].Intake.Mode, IntakeModeManual)
	}
	if cfg.Projects[0].Intake.AssignedTo != "" {
		t.Errorf("Intake.AssignedTo = %q, want empty (manual mode does not default it)", cfg.Projects[0].Intake.AssignedTo)
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
			name: "duplicate project name",
			body: minimalValidTOML + `
[[projects]]
name = "zing"
repo = "git@github.com:x/other.git"
path = "/home/peter/other"
tracker = "github"

[projects.commands]
test = "go test ./..."
lint = "golangci-lint run"
`,
			want: `zing.toml: duplicate project name "zing": each project's name must be unique`,
		},
		{
			name: "bad project intake.mode",
			body: minimalValidTOML + "\n[projects.intake]\nmode = \"sometimes\"\n",
			want: "zing.toml: project zing: intake.mode must be auto or manual",
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
			name: "budget.agent_minutes_per_ticket zero",
			body: minimalValidTOML + "\n[budget]\nagent_minutes_per_ticket = 0\n",
			want: wantBudgetMinutesError,
		},
		{
			name: "budget.agent_minutes_per_ticket negative",
			body: minimalValidTOML + "\n[budget]\nagent_minutes_per_ticket = -1\n",
			want: wantBudgetMinutesError,
		},
		{
			name: "budget.agent_minutes_per_ticket one past the one-year cap",
			body: minimalValidTOML + "\n[budget]\nagent_minutes_per_ticket = 525601\n",
			want: wantBudgetMinutesError,
		},
		{
			// One past math.MaxInt64/60 (153722867280912930): large enough
			// that converting it to a time.Duration in minutes would overflow
			// int64 nanoseconds, so this must fail here, at load, rather than
			// wrap silently later at the *time.Duration(minutes) conversion.
			name: "budget.agent_minutes_per_ticket large enough to overflow a duration in minutes",
			body: minimalValidTOML + "\n[budget]\nagent_minutes_per_ticket = 153722867280912931\n",
			want: wantBudgetMinutesError,
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

// TestLoad_BudgetAgentMinutesPerTicketBoundaryValuesLoad proves the two
// closed-range endpoints and the documented default all load cleanly
// (design section 4.4): the range check must reject only outside [1,
// 525600], never the boundary itself.
func TestLoad_BudgetAgentMinutesPerTicketBoundaryValuesLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want int
	}{
		{"minimum", minimalValidTOML + "\n[budget]\nagent_minutes_per_ticket = 1\n", 1},
		{"maximum", minimalValidTOML + "\n[budget]\nagent_minutes_per_ticket = 525600\n", 525600},
		{"default when the key is absent", minimalValidTOML, 240},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(writeTOML(t, tt.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Budget.AgentMinutesPerTicket != tt.want {
				t.Errorf("Budget.AgentMinutesPerTicket = %d, want %d", cfg.Budget.AgentMinutesPerTicket, tt.want)
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

// testAppendedProject is a valid, fully-specified Project every AppendProject
// test below appends, so a fixture missing a field is unambiguously testing
// that absence.
var testAppendedProject = Project{
	Name: "appended", Repo: "git@github.com:x/appended.git", Path: "/home/peter/appended",
	Tracker: testTracker, DefaultBranch: "main",
	Commands: Commands{Test: testCommandTest, Lint: testCommandLint},
}

// TestAppendProject_AppendsAndLoadsBack proves AppendProject's write half and
// Load's read half agree: a project appended to an existing, project-less
// config comes back out of the next Load with every field intact.
func TestAppendProject_AppendsAndLoadsBack(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, "user = \"peter\"\ngithub_token = \""+testGitHubToken+"\"\n")

	if err := AppendProject(path, testAppendedProject); err != nil {
		t.Fatalf("AppendProject: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after AppendProject: %v", err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects = %+v, want exactly one", cfg.Projects)
	}
	// Intake.AssignedTo defaults to the top-level user on Load, and
	// Intake.Mode defaults to "auto", since testAppendedProject leaves both
	// unset; every other field must come back exactly as appended.
	want := testAppendedProject
	want.Intake.AssignedTo = testUser
	want.Intake.Mode = IntakeModeAuto
	if got := cfg.Projects[0]; got != want {
		t.Errorf("appended project = %+v, want %+v", got, want)
	}
}

// TestAppendProject_WritesFileAt0600 proves AppendProject's file lands at
// 0600, since it carries github_token (PKG5-PLAN.md section 9).
func TestAppendProject_WritesFileAt0600(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, minimalValidTOML)

	if err := AppendProject(path, testAppendedProject); err != nil {
		t.Fatalf("AppendProject: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode after AppendProject = %o, want 0600", perm)
	}
}

// TestAppendProject_PreservesExplicitZeroAndEmptyValues proves the fix this
// exists for: an earlier "zing project add" re-marshaled and rewrote the
// whole Config, so an explicitly-set budget.usage_hold_percent = 0 or
// merge.manual_paths = [] came back out of that Config indistinguishable
// from a field zing.toml had never mentioned, and the rewrite silently
// dropped it. AppendProject never touches existing bytes, so both survive a
// project add unchanged.
func TestAppendProject_PreservesExplicitZeroAndEmptyValues(t *testing.T) {
	t.Parallel()

	const body = `
user = "peter"
github_token = "` + testGitHubToken + `"

[budget]
usage_hold_percent = 0

[merge]
manual_paths = []

[[projects]]
name = "existing"
repo = "git@github.com:x/existing.git"
path = "/home/peter/existing"
tracker = "github"

[projects.commands]
test = "go test ./..."
lint = "golangci-lint run"
`
	path := writeTOML(t, body)

	if err := AppendProject(path, testAppendedProject); err != nil {
		t.Fatalf("AppendProject: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after AppendProject: %v", err)
	}
	if cfg.Budget.UsageHoldPercent != 0 {
		t.Errorf("Budget.UsageHoldPercent = %d, want the explicit 0 to survive", cfg.Budget.UsageHoldPercent)
	}
	if len(cfg.Merge.ManualPaths) != 0 {
		t.Errorf("Merge.ManualPaths = %v, want the explicit empty slice to survive", cfg.Merge.ManualPaths)
	}
	if len(cfg.Projects) != 2 {
		t.Fatalf("Projects = %+v, want two", cfg.Projects)
	}
	if cfg.Projects[0].Name != "existing" {
		t.Errorf("Projects[0].Name = %q, want %q", cfg.Projects[0].Name, "existing")
	}
	if cfg.Projects[1].Name != testAppendedProject.Name {
		t.Errorf("Projects[1].Name = %q, want %q", cfg.Projects[1].Name, testAppendedProject.Name)
	}
}

// TestAppendProject_SecondProjectDoesNotDisturbFirst proves AppendProject
// changes nothing about a project already on record when a second one is
// appended after it.
func TestAppendProject_SecondProjectDoesNotDisturbFirst(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, minimalValidTOML)

	before, err := Load(path)
	if err != nil {
		t.Fatalf("Load before AppendProject: %v", err)
	}

	if appendErr := AppendProject(path, testAppendedProject); appendErr != nil {
		t.Fatalf("AppendProject: %v", appendErr)
	}

	after, err := Load(path)
	if err != nil {
		t.Fatalf("Load after AppendProject: %v", err)
	}
	if len(after.Projects) != 2 {
		t.Fatalf("Projects = %+v, want two", after.Projects)
	}
	if after.Projects[0] != before.Projects[0] {
		t.Errorf("first project changed after appending a second: got %+v, want %+v", after.Projects[0], before.Projects[0])
	}
	if after.Projects[1].Name != testAppendedProject.Name {
		t.Errorf("Projects[1].Name = %q, want %q", after.Projects[1].Name, testAppendedProject.Name)
	}
}

// TestAppendProject_MissingFileIsAnErrorAndCreatesNothing proves AppendProject
// only ever appends to a file that already exists; a missing path is an
// error, not a fresh file created out of just the one project.
func TestAppendProject_MissingFileIsAnErrorAndCreatesNothing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "zing.toml")

	if err := AppendProject(path, testAppendedProject); err == nil {
		t.Fatal("AppendProject() = nil, want an error for a missing zing.toml")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("AppendProject created a file at path, want none for a missing zing.toml")
	}
}

// TestAppendProject_RemovesPreexistingEmptyProjectsArray proves the PR
// review fix: a zing.toml bootstrapped through LoadForAdd's zero-projects
// path can carry an explicit "projects = []" key. Appending "[[projects]]"
// onto that unchanged would redefine "projects" twice, which the next Load
// then rejects. AppendProject must strip that inline empty array first, in
// every realistic spelling, so the appended project becomes the first (and
// only) element and the file still Loads.
func TestAppendProject_RemovesPreexistingEmptyProjectsArray(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"single line, spaced", "user = \"x\"\ngithub_token = \"y\"\nprojects = []\n"},
		{"single line, no spaces", "user = \"x\"\ngithub_token = \"y\"\nprojects=[]\n"},
		{"single line, extra internal spaces", "user = \"x\"\ngithub_token = \"y\"\nprojects  =   [   ]\n"},
		{"multiline empty array", "user = \"x\"\ngithub_token = \"y\"\nprojects = [\n]\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			path := writeTOML(t, c.body)

			if err := AppendProject(path, testAppendedProject); err != nil {
				t.Fatalf("AppendProject: %v", err)
			}

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load after AppendProject: %v", err)
			}
			if len(cfg.Projects) != 1 {
				t.Fatalf("Projects = %+v, want exactly one", cfg.Projects)
			}
			if cfg.Projects[0].Name != testAppendedProject.Name {
				t.Errorf("Projects[0].Name = %q, want %q", cfg.Projects[0].Name, testAppendedProject.Name)
			}
		})
	}
}

// TestAppendProject_LeavesAnExistingProjectsTableAlone is the regression
// case alongside the empty-array fix above: a file whose "projects" key is
// already an array of tables (at least one real [[projects]] entry, not an
// inline empty array) must be left completely alone by
// emptyProjectsArrayPattern, and a second project must still append cleanly
// onto it.
func TestAppendProject_LeavesAnExistingProjectsTableAlone(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, minimalValidTOML)

	if err := AppendProject(path, testAppendedProject); err != nil {
		t.Fatalf("AppendProject: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after AppendProject: %v", err)
	}
	if len(cfg.Projects) != 2 {
		t.Fatalf("Projects = %+v, want two", cfg.Projects)
	}
	if cfg.Projects[0].Name != testZingProjectName {
		t.Errorf("Projects[0].Name = %q, want %q (the original entry, untouched)", cfg.Projects[0].Name, testZingProjectName)
	}
	if cfg.Projects[1].Name != testAppendedProject.Name {
		t.Errorf("Projects[1].Name = %q, want %q", cfg.Projects[1].Name, testAppendedProject.Name)
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

// TestLoad_PathIsADirectoryIsRejectedWithoutChmod proves the PR review fix
// to repairFileMode: a config path that is a directory (or any non-regular
// file) is reported as an error rather than chmod'd. Before the fix, a
// group- or other-readable directory (0755 here, whose group/other bits
// overlap permissiveMode) would have been chmod'd to 0600, stripping the
// execute bit a directory needs to be traversable at all.
func TestLoad_PathIsADirectoryIsRejectedWithoutChmod(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "zing.toml")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load() = nil, want an error when the config path is a directory")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("directory mode = %o after Load, want unchanged 0755 (repairFileMode must not chmod a non-regular file)", perm)
	}
}

// TestAppendProject_InvalidCombinedConfigLeavesFileUnchanged proves the PR
// review fix: a pre-existing shape emptyProjectsArrayPattern does not strip
// (here a non-empty inline projects array) would duplicate the projects key.
// AppendProject must decode the combined bytes first and, on failure, return
// an error and leave the token-bearing file byte-for-byte unchanged.
func TestAppendProject_InvalidCombinedConfigLeavesFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zing.toml")
	original := `user = "peter"
github_token = "tok"
projects = [{ name = "a", repo = "git@github.com:x/a.git", path = "/p/a", tracker = "github", commands = { test = "t", lint = "l" } }]
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	p := Project{
		Name: "b", Repo: "git@github.com:x/b.git", Path: "/p/b", Tracker: testTracker,
		Commands: Commands{Test: testCommandTest, Lint: testCommandLint},
	}
	if err := AppendProject(path, p); err == nil {
		t.Fatal("AppendProject: expected an error for a combined config that duplicates the projects key, got nil")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != original {
		t.Errorf("AppendProject changed the file on a validation failure:\n%s", after)
	}
}
