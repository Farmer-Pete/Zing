package machine

import (
	"testing"
	"testing/fstest"

	zing "zing"
)

// Path literals repeated across this file's fixtures.
const (
	machineTOMLPath = "machine.toml"
	stubPromptPath  = "classify.md"
)

func TestLoad_RealMachineTOMLLoadsClean(t *testing.T) {
	t.Parallel()

	m, err := Load(zing.Assets, machineTOMLPath)
	if err != nil {
		t.Fatalf("Load(real machine.toml): %v", err)
	}

	if m.Version != 1 {
		t.Errorf("Version = %d, want 1", m.Version)
	}
	if len(m.Jobs) != 9 {
		t.Errorf("len(Jobs) = %d, want 9", len(m.Jobs))
	}

	// classify's prompt is a bare string.
	if got := m.Jobs["classify"].Prompt.Single; got != "prompts/classify.md" {
		t.Errorf("classify.Prompt.Single = %q, want prompts/classify.md", got)
	}

	// planning's prompt is a {feature, bug} pair.
	planning := m.Jobs["planning"]
	if planning.Prompt.Feature != "prompts/planning-feature.md" || planning.Prompt.Bug != "prompts/planning-bug.md" {
		t.Errorf("planning.Prompt = %+v, want the feature/bug pair", planning.Prompt)
	}

	// side sets no outcomes key, so it defaults to ["ok"].
	side := m.Jobs["side"]
	if len(side.Outcomes) != 1 || side.Outcomes[0] != "ok" {
		t.Errorf("side.Outcomes = %v, want [ok] (the absent-key default)", side.Outcomes)
	}

	// build sets max_resumes explicitly.
	if got := m.Jobs["build"].MaxResumes; got != 3 {
		t.Errorf("build.MaxResumes = %d, want 3", got)
	}

	// classify sets no max_resumes key, so it defaults to 1.
	if got := m.Jobs["classify"].MaxResumes; got != 1 {
		t.Errorf("classify.MaxResumes = %d, want 1 (the absent-key default)", got)
	}
}

// validJobFragment is a minimal, fully valid job body. A test appends one
// more line to exercise a single validation rule that runs after every
// field validJobFragment already sets.
const validJobFragment = `
model = "sonnet"
runtime = "claude"
tools = ["read"]
prompt = "classify.md"
timeout_minutes = 5
`

func machineFixture(t *testing.T, jobBody string) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		machineTOMLPath: &fstest.MapFile{Data: []byte("version = 1\n\n[jobs.test]\n" + jobBody + "\n" +
			"[states]\norder = [\"queued\"]\nterminal = [\"done\"]\n")},
		stubPromptPath: &fstest.MapFile{Data: []byte("stub\n")},
	}
}

func TestLoad_JobFieldValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		jobBody string
		want    string
	}{
		{
			name:    "bad model",
			jobBody: `model = "bogus"` + "\n",
			want:    "machine.toml: job test: model: must be one of sonnet, opus, fable, codex",
		},
		{
			name:    "bad runtime",
			jobBody: "model = \"sonnet\"\nruntime = \"bogus\"\n",
			want:    "machine.toml: job test: runtime: must be one of claude, codex, fake",
		},
		{
			name:    "unknown tool",
			jobBody: "model = \"sonnet\"\nruntime = \"claude\"\ntools = [\"nope\"]\n",
			want:    "machine.toml: job test: tools: unknown tool nope",
		},
		{
			name: "missing prompt path",
			jobBody: `model = "sonnet"
runtime = "claude"
tools = ["read"]
prompt = "does-not-exist.md"
timeout_minutes = 5
`,
			want: "machine.toml: job test: prompt: file not found: does-not-exist.md",
		},
		{
			name:    "bad max_loops (explicit zero)",
			jobBody: validJobFragment + "\nmax_loops = 0\n",
			want:    "machine.toml: job test: max_loops: must be 1 to 5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(machineFixture(t, tt.jobBody), machineTOMLPath)
			if err == nil {
				t.Fatalf("Load() = nil, want error %q", tt.want)
			}
			if err.Error() != tt.want {
				t.Errorf("Load() = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestLoad_VersionMustBeOne(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		machineTOMLPath: &fstest.MapFile{Data: []byte(`version = 2

[jobs.test]
model = "sonnet"
runtime = "claude"
tools = ["read"]
prompt = "classify.md"
timeout_minutes = 5

[states]
order = ["queued"]
terminal = ["done"]
`)},
		stubPromptPath: &fstest.MapFile{Data: []byte("stub\n")},
	}

	_, err := Load(fsys, machineTOMLPath)
	want := "machine.toml: version: must be 1"
	if err == nil || err.Error() != want {
		t.Errorf("Load() = %v, want %q", err, want)
	}
}

func TestPromptRef_AcceptsStringAndFeatureBugPair(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		machineTOMLPath: &fstest.MapFile{Data: []byte(`version = 1

[jobs.single]
model = "sonnet"
runtime = "claude"
tools = ["read"]
prompt = "classify.md"
timeout_minutes = 5

[jobs.pair]
model = "sonnet"
runtime = "claude"
tools = ["read"]
prompt = { feature = "classify.md", bug = "classify.md" }
timeout_minutes = 5

[states]
order = ["queued"]
terminal = ["done"]
`)},
		stubPromptPath: &fstest.MapFile{Data: []byte("stub\n")},
	}

	m, err := Load(fsys, machineTOMLPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Jobs["single"].Prompt.Single != stubPromptPath {
		t.Errorf("single.Prompt.Single = %q, want classify.md", m.Jobs["single"].Prompt.Single)
	}
	if m.Jobs["pair"].Prompt.Feature != stubPromptPath || m.Jobs["pair"].Prompt.Bug != stubPromptPath {
		t.Errorf("pair.Prompt = %+v, want feature and bug both classify.md", m.Jobs["pair"].Prompt)
	}
}

func TestPromptRef_PartialPairIsInvalid(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		machineTOMLPath: &fstest.MapFile{Data: []byte(`version = 1

[jobs.test]
model = "sonnet"
runtime = "claude"
tools = ["read"]
prompt = { feature = "classify.md" }
timeout_minutes = 5

[states]
order = ["queued"]
terminal = ["done"]
`)},
		stubPromptPath: &fstest.MapFile{Data: []byte("stub\n")},
	}

	_, err := Load(fsys, machineTOMLPath)
	want := "machine.toml: job test: prompt: must be a path or a {feature,bug} pair"
	if err == nil || err.Error() != want {
		t.Errorf("Load() = %v, want %q", err, want)
	}
}
