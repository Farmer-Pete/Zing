package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const testUser = "peter"

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
		User: testUser,
		Console: Console{
			Bind: []string{"127.0.0.1", "tailscale"},
			Port: 7420,
			// PushToken is randomly generated; checked separately below.
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
				Self:     false,
				Intake:   Intake{AssignedTo: testUser}, // defaults to the top-level user
				Commands: Commands{Test: "go test ./...", Lint: "golangci-lint run"},
			},
		},
	}

	if cfg.Console.PushToken == "" {
		t.Error("Console.PushToken was not generated")
	}
	gotToken := cfg.Console.PushToken
	cfg.Console.PushToken = ""

	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
	if len(gotToken) < 32 {
		t.Errorf("Console.PushToken = %q, too short for 32 random bytes base64-encoded", gotToken)
	}
}

func TestLoad_FullConfigKeepsExplicitValues(t *testing.T) {
	t.Parallel()

	const full = `
user = "peter"

[console]
bind = ["127.0.0.1"]
port = 8080
push_token = "explicit-token"

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
		User: testUser,
		Console: Console{
			Bind:      []string{"127.0.0.1"},
			Port:      8080,
			PushToken: "explicit-token",
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
				Self:     true,
				Intake:   Intake{AssignedTo: "someone-else"},
				Commands: Commands{Test: "go test ./...", Lint: "golangci-lint run"},
			},
		},
	}

	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoad_IntakeAssignedToDefaultsWhenExplicitlyEmpty(t *testing.T) {
	t.Parallel()

	const body = `
user = "peter"

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
			body: `user = "peter"`,
			want: "zing.toml: missing required key projects",
		},
		{
			name: "missing projects[0].name",
			body: `
user = "peter"

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
