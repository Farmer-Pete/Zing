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
	// testJobNameBuild is the real machine.toml's "build" job name
	// (goconst): both this file and prompts_test.go repeat it.
	testJobNameBuild = "build"
	// planningFeaturePromptPath and planningBugPromptPath are the real
	// machine.toml's planning prompt paths (goconst): this file and
	// prompts_test.go both repeat them.
	planningFeaturePromptPath = "prompts/planning-feature.md"
	planningBugPromptPath     = "prompts/planning-bug.md"
	// modelOpus is the real machine.toml's "opus" model name (goconst):
	// planning, review, and merge all use it.
	modelOpus = "opus"
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
	if len(m.Jobs) != 10 {
		t.Errorf("len(Jobs) = %d, want 10", len(m.Jobs))
	}

	// classify's prompt is a bare string.
	if got := m.Jobs["classify"].Prompt.Single; got != "prompts/classify.md" {
		t.Errorf("classify.Prompt.Single = %q, want prompts/classify.md", got)
	}

	// planning's prompt is a {feature, bug} pair.
	planning := m.Jobs["planning"]
	if planning.Prompt.Feature != planningFeaturePromptPath || planning.Prompt.Bug != planningBugPromptPath {
		t.Errorf("planning.Prompt = %+v, want the feature/bug pair", planning.Prompt)
	}
	if planning.Model != modelOpus {
		t.Errorf("planning.Model = %q, want opus", planning.Model)
	}
	if got := m.Jobs["review"].Model; got != modelOpus {
		t.Errorf("review.Model = %q, want opus", got)
	}

	// side sets no outcomes key, so it defaults to ["ok"].
	side := m.Jobs["side"]
	if len(side.Outcomes) != 1 || side.Outcomes[0] != "ok" {
		t.Errorf("side.Outcomes = %v, want [ok] (the absent-key default)", side.Outcomes)
	}

	// build sets max_resumes explicitly.
	if got := m.Jobs[testJobNameBuild].MaxResumes; got != 3 {
		t.Errorf("build.MaxResumes = %d, want 3", got)
	}

	// classify sets no max_resumes key, so it defaults to 1.
	if got := m.Jobs["classify"].MaxResumes; got != 1 {
		t.Errorf("classify.MaxResumes = %d, want 1 (the absent-key default)", got)
	}

	// build and perimeter both run under the seatbelt sandbox; classify does
	// not (PKG8-PLAN.md section 4.5).
	if got := m.Jobs[testJobNameBuild].Sandbox; got != testJobNameBuild {
		t.Errorf("build.Sandbox = %q, want build", got)
	}
	if got := m.Jobs["perimeter"].Sandbox; got != testJobNameBuild {
		t.Errorf("perimeter.Sandbox = %q, want build", got)
	}
	if got := m.Jobs["classify"].Sandbox; got != "" {
		t.Errorf("classify.Sandbox = %q, want empty", got)
	}

	// respond runs under the readonly sandbox, bounded by max_loops 3
	// (D14's shared shipping counter) and the prose style file (D15).
	respond := m.Jobs["respond"]
	if respond.Sandbox != "readonly" {
		t.Errorf("respond.Sandbox = %q, want readonly", respond.Sandbox)
	}
	if respond.MaxLoops != 3 {
		t.Errorf("respond.MaxLoops = %d, want 3", respond.MaxLoops)
	}
	if len(respond.Style) != 1 || respond.Style[0] != "prompts/style/prose.md" {
		t.Errorf("respond.Style = %v, want [prompts/style/prose.md]", respond.Style)
	}

	// merge resolves a conflicting base branch merge (#64's ticket);
	// opus judgement, build's sandbox, and its own loop limits.
	merge := m.Jobs["merge"]
	if merge.Model != modelOpus {
		t.Errorf("merge.Model = %q, want opus", merge.Model)
	}
	if merge.Runtime != "claude" {
		t.Errorf("merge.Runtime = %q, want claude", merge.Runtime)
	}
	if merge.Prompt.Single != "prompts/merge.md" {
		t.Errorf("merge.Prompt.Single = %q, want prompts/merge.md", merge.Prompt.Single)
	}
	if merge.TimeoutMinutes != 45 {
		t.Errorf("merge.TimeoutMinutes = %d, want 45", merge.TimeoutMinutes)
	}
	if merge.MaxResumes != 6 {
		t.Errorf("merge.MaxResumes = %d, want 6", merge.MaxResumes)
	}
	if merge.MaxLoops != 2 {
		t.Errorf("merge.MaxLoops = %d, want 2", merge.MaxLoops)
	}
	if merge.CheckLoops != 5 {
		t.Errorf("merge.CheckLoops = %d, want 5", merge.CheckLoops)
	}
	if len(merge.Outcomes) != 1 || merge.Outcomes[0] != "ok" {
		t.Errorf("merge.Outcomes = %v, want [ok]", merge.Outcomes)
	}
	if merge.Sandbox != testJobNameBuild {
		t.Errorf("merge.Sandbox = %q, want build", merge.Sandbox)
	}
}

// TestJobSandboxKey proves the sandbox key's own validation (PKG8-PLAN.md
// section 4.5): absent or "build" both load clean, and any other value is
// rejected with the exact error text.
func TestJobSandboxKey(t *testing.T) {
	t.Parallel()

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		m, err := Load(machineFixture(t, validJobFragment), machineTOMLPath)
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if got := m.Jobs["test"].Sandbox; got != "" {
			t.Errorf("Sandbox = %q, want empty", got)
		}
	})

	t.Run(testJobNameBuild, func(t *testing.T) {
		t.Parallel()
		m, err := Load(machineFixture(t, validJobFragment+"\nsandbox = \"build\"\n"), machineTOMLPath)
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if got := m.Jobs["test"].Sandbox; got != testJobNameBuild {
			t.Errorf("Sandbox = %q, want build", got)
		}
	})

	t.Run("readonly", func(t *testing.T) {
		t.Parallel()
		m, err := Load(machineFixture(t, validJobFragment+"\nsandbox = \"readonly\"\n"), machineTOMLPath)
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if got := m.Jobs["test"].Sandbox; got != "readonly" {
			t.Errorf("Sandbox = %q, want readonly", got)
		}
	})

	t.Run("judge", func(t *testing.T) {
		t.Parallel()
		m, err := Load(machineFixture(t, validJobFragment+"\nsandbox = \"judge\"\n"), machineTOMLPath)
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if got := m.Jobs["test"].Sandbox; got != "judge" {
			t.Errorf("Sandbox = %q, want judge", got)
		}
	})

	t.Run("anything else", func(t *testing.T) {
		t.Parallel()
		_, err := Load(machineFixture(t, validJobFragment+"\nsandbox = \"bogus\"\n"), machineTOMLPath)
		if err == nil {
			t.Fatal("Load() = nil, want an error")
		}
		want := "machine.toml: job test: sandbox: must be absent, build, readonly, or judge"
		if err.Error() != want {
			t.Errorf("Load() = %q, want %q", err.Error(), want)
		}
	})
}

// TestLoad_UnknownKeyIsRejected proves the machine.toml unknown-key check
// (mirroring config.go's) rejects a misspelled key, and does not
// false-positive on a job's well-formed "prompt" table.
func TestLoad_UnknownKeyIsRejected(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		machineTOMLPath: &fstest.MapFile{Data: []byte(`version = 1

[jobs.test]
model = "sonnet"
runtime = "claude"
tools = ["read"]
prompt = "classify.md"
timeout_minutes = 5
max_resume = 30

[states]
order = ["queued"]
terminal = ["done"]
`)},
		stubPromptPath: &fstest.MapFile{Data: []byte("stub\n")},
	}

	_, err := Load(fsys, machineTOMLPath)
	want := "machine.toml: unknown key jobs.test.max_resume"
	if err == nil || err.Error() != want {
		t.Errorf("Load() = %v, want %q", err, want)
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

// TestLoad_CheckLoopsDefaultsToFive proves the build job's check loop cap
// (#55) is 5 in the repository's machine.toml and 5 when the key is absent.
func TestLoad_CheckLoopsDefaultsToFive(t *testing.T) {
	t.Parallel()

	m, err := Load(zing.Assets, machineTOMLPath)
	if err != nil {
		t.Fatalf("Load(real machine.toml): %v", err)
	}
	if got := m.Jobs[testJobNameBuild].CheckLoops; got != 5 {
		t.Errorf("build.CheckLoops = %d, want 5", got)
	}

	m, err = Load(machineFixture(t, validJobFragment), machineTOMLPath)
	if err != nil {
		t.Fatalf("Load(fragment): %v", err)
	}
	if got := m.Jobs["test"].CheckLoops; got != 5 {
		t.Errorf("CheckLoops with no key = %d, want 5 (the absent-key default)", got)
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
		{
			name:    "bad max_loops (out of range)",
			jobBody: validJobFragment + "\nmax_loops = 6\n",
			want:    "machine.toml: job test: max_loops: must be 1 to 5",
		},
		{
			name:    "bad check_loops (zero)",
			jobBody: validJobFragment + "\ncheck_loops = 0\n",
			want:    "machine.toml: job test: check_loops: must be 1 to 20",
		},
		{
			name:    "bad check_loops (21)",
			jobBody: validJobFragment + "\ncheck_loops = 21\n",
			want:    "machine.toml: job test: check_loops: must be 1 to 20",
		},
		{
			name:    "bad session",
			jobBody: "model = \"sonnet\"\nruntime = \"claude\"\nsession = \"bogus\"\n",
			want:    "machine.toml: job test: session: must be absent or long",
		},
		{
			name:    "bad per",
			jobBody: "model = \"sonnet\"\nruntime = \"claude\"\nper = \"bogus\"\n",
			want:    "machine.toml: job test: per: must be absent, task, or lens",
		},
		{
			name:    "bad worktree",
			jobBody: "model = \"sonnet\"\nruntime = \"claude\"\nworktree = \"bogus\"\n",
			want:    "machine.toml: job test: worktree: must be absent or sparse",
		},
		{
			name:    "bad max_resumes (out of range)",
			jobBody: "model = \"sonnet\"\nruntime = \"claude\"\nmax_resumes = 21\n",
			want:    "machine.toml: job test: max_resumes: must be 0 to 20",
		},
		{
			name:    "bad timeout_minutes (out of range)",
			jobBody: "model = \"sonnet\"\nruntime = \"claude\"\ntimeout_minutes = 300\n",
			want:    "machine.toml: job test: timeout_minutes: must be 1 to 240",
		},
		{
			name:    "empty outcomes",
			jobBody: validJobFragment + "\noutcomes = []\n",
			want:    "machine.toml: job test: outcomes: must not be empty",
		},
		{
			name:    "unknown outcome",
			jobBody: validJobFragment + "\noutcomes = [\"bogus\"]\n",
			want:    "machine.toml: job test: outcomes: unknown outcome bogus",
		},
		{
			name:    "duplicate outcome",
			jobBody: validJobFragment + "\noutcomes = [\"ok\", \"ok\"]\n",
			want:    "machine.toml: job test: outcomes: duplicate ok",
		},
		{
			name:    "universal outcome listed",
			jobBody: validJobFragment + "\noutcomes = [\"question\"]\n",
			want:    "machine.toml: job test: outcomes: question and error are universal, not listed",
		},
		{
			// Fix 3 precedence: "question" (universal-eligible) sits before
			// "bogus" (unknown) in the list, but the unknown check runs as a
			// whole-list pass before the universal check, so the unknown
			// error wins regardless of list position.
			name:    "outcomes precedence: unknown beats a universal value earlier in the list",
			jobBody: validJobFragment + "\noutcomes = [\"question\", \"bogus\"]\n",
			want:    "machine.toml: job test: outcomes: unknown outcome bogus",
		},
		{
			name:    "prompt table with an unknown key",
			jobBody: "model = \"sonnet\"\nruntime = \"claude\"\ntools = [\"read\"]\ntimeout_minutes = 5\nprompt = { color = \"x\" }\n",
			want:    "machine.toml: job test: prompt: must be a path or a {feature,bug} pair",
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

// TestLoad_PathResolvingToADirectoryIsRejected proves a prompt/style/lens
// path that exists but is a directory, not a regular file, is rejected with
// "not a regular file", distinct from the "file not found" case.
func TestLoad_PathResolvingToADirectoryIsRejected(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		machineTOMLPath: &fstest.MapFile{Data: []byte(`version = 1

[jobs.test]
model = "sonnet"
runtime = "claude"
tools = ["read"]
prompt = "classify.md"
style = ["adir"]
timeout_minutes = 5

[states]
order = ["queued"]
terminal = ["done"]
`)},
		stubPromptPath: &fstest.MapFile{Data: []byte("stub\n")},
		// No file named "adir" itself; fstest.MapFS treats "adir" as an
		// implicit directory because a file exists at "adir/inner.md".
		"adir/inner.md": &fstest.MapFile{Data: []byte("x\n")},
	}

	_, err := Load(fsys, machineTOMLPath)
	want := "machine.toml: job test: style: not a regular file: adir"
	if err == nil || err.Error() != want {
		t.Errorf("Load() = %v, want %q", err, want)
	}
}

// TestLoad_StatesValidation covers the machine-level states.order and
// states.terminal checks: must not be empty, every name must be a known
// TicketState, and no duplicate within one list.
func TestLoad_StatesValidation(t *testing.T) {
	t.Parallel()

	validJob := `
[jobs.test]
model = "sonnet"
runtime = "claude"
tools = ["read"]
prompt = "classify.md"
timeout_minutes = 5
`

	tests := []struct {
		name       string
		statesBody string
		want       string
	}{
		{
			name:       "empty order",
			statesBody: "order = []\nterminal = [\"done\"]\n",
			want:       "machine.toml: states.order: must not be empty",
		},
		{
			name:       "empty terminal",
			statesBody: "order = [\"queued\"]\nterminal = []\n",
			want:       "machine.toml: states.terminal: must not be empty",
		},
		{
			name:       "unknown state in order",
			statesBody: "order = [\"queued\", \"bogus\"]\nterminal = [\"done\"]\n",
			want:       "machine.toml: states.order: unknown state bogus",
		},
		{
			name:       "unknown state in terminal",
			statesBody: "order = [\"queued\"]\nterminal = [\"bogus\"]\n",
			want:       "machine.toml: states.terminal: unknown state bogus",
		},
		{
			name:       "duplicate state in order",
			statesBody: "order = [\"queued\", \"queued\"]\nterminal = [\"done\"]\n",
			want:       "machine.toml: states.order: duplicate state queued",
		},
		{
			name:       "duplicate state in terminal",
			statesBody: "order = [\"queued\"]\nterminal = [\"done\", \"done\"]\n",
			want:       "machine.toml: states.terminal: duplicate state done",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := fstest.MapFS{
				machineTOMLPath: &fstest.MapFile{Data: []byte(
					"version = 1\n" + validJob + "\n[states]\n" + tt.statesBody,
				)},
				stubPromptPath: &fstest.MapFile{Data: []byte("stub\n")},
			}
			_, err := Load(fsys, machineTOMLPath)
			if err == nil || err.Error() != tt.want {
				t.Errorf("Load() = %v, want %q", err, tt.want)
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

// TestLoad_IdleMinutesAndTimeoutRetries covers the idle_minutes and
// timeout_retries fields: both default to 0 when absent, idle_minutes is
// claude-only and must be at least 1 and less than timeout_minutes, and
// timeout_retries must be 0 or 1.
func TestLoad_IdleMinutesAndTimeoutRetries(t *testing.T) {
	t.Parallel()

	t.Run("idle_minutes and timeout_retries set", func(t *testing.T) {
		t.Parallel()
		m, err := Load(machineFixture(t, validJobFragment+"\nidle_minutes = 2\ntimeout_retries = 1\n"), machineTOMLPath)
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if got := m.Jobs["test"].IdleMinutes; got != 2 {
			t.Errorf("IdleMinutes = %d, want 2", got)
		}
		if got := m.Jobs["test"].TimeoutRetries; got != 1 {
			t.Errorf("TimeoutRetries = %d, want 1", got)
		}
	})

	t.Run("absent keys default to zero", func(t *testing.T) {
		t.Parallel()
		m, err := Load(machineFixture(t, validJobFragment), machineTOMLPath)
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if got := m.Jobs["test"].IdleMinutes; got != 0 {
			t.Errorf("IdleMinutes = %d, want 0 (the absent-key default)", got)
		}
		if got := m.Jobs["test"].TimeoutRetries; got != 0 {
			t.Errorf("TimeoutRetries = %d, want 0 (the absent-key default)", got)
		}
	})

	t.Run("idle_minutes zero", func(t *testing.T) {
		t.Parallel()
		_, err := Load(machineFixture(t, validJobFragment+"\nidle_minutes = 0\n"), machineTOMLPath)
		want := "machine.toml: job test: idle_minutes: must be at least 1 and less than timeout_minutes"
		if err == nil || err.Error() != want {
			t.Errorf("Load() = %v, want %q", err, want)
		}
	})

	t.Run("idle_minutes not less than timeout_minutes", func(t *testing.T) {
		t.Parallel()
		_, err := Load(machineFixture(t, validJobFragment+"\nidle_minutes = 5\n"), machineTOMLPath)
		want := "machine.toml: job test: idle_minutes: must be at least 1 and less than timeout_minutes"
		if err == nil || err.Error() != want {
			t.Errorf("Load() = %v, want %q", err, want)
		}
	})

	t.Run("idle_minutes on a codex job", func(t *testing.T) {
		t.Parallel()
		_, err := Load(machineFixture(t, "model = \"codex\"\nruntime = \"codex\"\ntools = [\"read\"]\nprompt = \"classify.md\"\ntimeout_minutes = 5\nidle_minutes = 2\n"), machineTOMLPath)
		want := "machine.toml: job test: idle_minutes: only the claude runtime supports it"
		if err == nil || err.Error() != want {
			t.Errorf("Load() = %v, want %q", err, want)
		}
	})

	t.Run("timeout_retries out of range high", func(t *testing.T) {
		t.Parallel()
		_, err := Load(machineFixture(t, validJobFragment+"\ntimeout_retries = 2\n"), machineTOMLPath)
		want := "machine.toml: job test: timeout_retries: must be 0 or 1"
		if err == nil || err.Error() != want {
			t.Errorf("Load() = %v, want %q", err, want)
		}
	})

	t.Run("timeout_retries out of range low", func(t *testing.T) {
		t.Parallel()
		_, err := Load(machineFixture(t, validJobFragment+"\ntimeout_retries = -1\n"), machineTOMLPath)
		want := "machine.toml: job test: timeout_retries: must be 0 or 1"
		if err == nil || err.Error() != want {
			t.Errorf("Load() = %v, want %q", err, want)
		}
	})
}

// TestLoad_ShippedMachineTimeoutKeys proves the real machine.toml sets
// idle_minutes = 2 and timeout_retries = 1 on classify and perimeter only;
// every other job keeps both at their absent-key default of 0.
func TestLoad_ShippedMachineTimeoutKeys(t *testing.T) {
	t.Parallel()

	m, err := Load(zing.Assets, machineTOMLPath)
	if err != nil {
		t.Fatalf("Load(real machine.toml): %v", err)
	}

	for _, name := range []string{"classify", "perimeter"} {
		job := m.Jobs[name]
		if job.IdleMinutes != 2 {
			t.Errorf("%s.IdleMinutes = %d, want 2", name, job.IdleMinutes)
		}
		if job.TimeoutRetries != 1 {
			t.Errorf("%s.TimeoutRetries = %d, want 1", name, job.TimeoutRetries)
		}
	}

	for _, name := range []string{testJobNameBuild, "merge", "planning", "review", "respond", "side"} {
		job := m.Jobs[name]
		if job.IdleMinutes != 0 {
			t.Errorf("%s.IdleMinutes = %d, want 0", name, job.IdleMinutes)
		}
		if job.TimeoutRetries != 0 {
			t.Errorf("%s.TimeoutRetries = %d, want 0", name, job.TimeoutRetries)
		}
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
