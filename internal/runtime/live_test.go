// live_test.go is the opt-in live CLI harness (design section 13 task 15,
// D11, D18): the one place this repository proves three claims against the
// real, pinned claude binary instead of the fake_claude.sh script the rest
// of this package's tests drive.
//
//  1. TestLive_Classify: a real classify turn, on a tiny fixture repo,
//     returns a bug or feature verdict and a usable session id.
//  2. TestLive_HookSuppression: a project's own .claude/settings.json hook
//     does not fire under the exact argv claudeArgv builds (D18's
//     --restricted), proving host isolation rather than assuming it.
//  3. TestLive_DontAskDenial: with a tool list that excludes Write, asking
//     the model to write a file completes cleanly with no prompt, no
//     hang, and no file.
//
// Every test here spends the owner's real Claude usage; none of them may
// run by default, in CI, or from any other test in this repository. Each
// checks ZING_LIVE_CLI itself and skips unless it is exactly "1". Run them
// explicitly, deliberately, one at a time or all together:
//
//	ZING_LIVE_CLI=1 go test ./internal/runtime/ -run Live -v
//
// ZING_LIVE_MODEL overrides the model (default claude-sonnet-5). Every
// test removes its own temp directories and never touches the owner's
// real Zing store; none of them print the assembled prompt or the
// response body, live or not.
package runtime

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	zing "zing"
	"zing/internal/machine"
	"zing/internal/prompt"
	"zing/internal/response"
)

// skipUnlessLive is the gate every test in this file opens with (design
// section 13 task 15): ZING_LIVE_CLI must be exactly "1", set by the owner,
// never by this harness or by CI.
func skipUnlessLive(t *testing.T) {
	t.Helper()
	if os.Getenv("ZING_LIVE_CLI") != "1" {
		t.Skip("set ZING_LIVE_CLI=1 to run against the real claude CLI")
	}
}

// liveModel is the model every live test runs against: ZING_LIVE_MODEL
// when set, else the pinned default the task names.
func liveModel() string {
	if m := os.Getenv("ZING_LIVE_MODEL"); m != "" {
		return m
	}
	return "claude-sonnet-5"
}

// fakeRepo returns a temp directory holding one small Go file, standing in
// for the project checkout a real job's WorkDir would otherwise be. t
// removes it at test end.
func fakeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	const src = "package fixture\n\nfunc Add(a, b int) int { return a + b }\n"
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture repo: %v", err)
	}
	return dir
}

// classifySchemas renders the classify job's schema order (design section
// 4.2): bug, feature, then the two universal outcomes, question and
// error.
func classifySchemas(t *testing.T) []string {
	t.Helper()
	outcomes := []response.Outcome{
		response.OutcomeBug, response.OutcomeFeature,
		response.OutcomeQuestion, response.OutcomeError,
	}
	schemas := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		s, err := response.RenderTemplate(response.JobClassify, o)
		if err != nil {
			t.Fatalf("render template classify/%s: %v", o, err)
		}
		schemas = append(schemas, s)
	}
	return schemas
}

// classifyPrompt reads the pinned classify.md prompt out of the repo's own
// embed (zing.Assets, the same tree machine.toml's prompt paths resolve
// against) and assembles it around ticket with the classify schema order,
// exactly as internal/job's runClassify does for a real ticket.
func classifyPrompt(t *testing.T, ticket string) string {
	t.Helper()
	promptText, err := fs.ReadFile(zing.Assets, "prompts/classify.md")
	if err != nil {
		t.Fatalf("read prompts/classify.md: %v", err)
	}
	in := prompt.ForClassify(string(promptText), ticket, nil)
	in.Schemas = classifySchemas(t)
	return prompt.Assemble(in)
}

// classifyTools reads machine.toml's own classify job tools, the same
// source runJob resolves a job's tools from, rather than hand-rolling a
// list that could drift from it.
func classifyTools(t *testing.T) []string {
	t.Helper()
	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("load machine.toml: %v", err)
	}
	job, ok := m.Jobs["classify"]
	if !ok {
		t.Fatal("machine.toml has no classify job")
	}
	return job.Tools
}

// runLive runs req through a fresh Claude runtime, under a context bounded
// by req.Timeout -- the same shape runJob gives a Runtime (design section
// 4.6): the timeout lives on the request for the record, but the context
// deadline is what actually bounds the process.
func runLive(t *testing.T, req RunRequest) (RunResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), req.Timeout)
	defer cancel()
	return NewClaude("").Run(ctx, req)
}

// TestLive_Classify is task 15(a): a real classify turn on a small fixture
// ticket, against a tiny fake repo, must return a usable session id and a
// bug or feature verdict -- the one outcome shape the fake runtime can
// never actually prove the real CLI produces.
func TestLive_Classify(t *testing.T) {
	skipUnlessLive(t)

	const ticket = "Add panics on overflow\n\n" +
		"fixture.Add(a, b int) returns a wrong (wrapped) result for two large " +
		"ints instead of reporting an error; expected behavior is undefined " +
		"today, so decide bug or feature from the ticket text alone."

	req := RunRequest{
		Job:     response.JobClassify,
		Model:   liveModel(),
		Prompt:  classifyPrompt(t, ticket),
		Tools:   classifyTools(t),
		WorkDir: fakeRepo(t),
		Timeout: 5 * time.Minute,
	}

	res, err := runLive(t, req)
	if err != nil {
		t.Fatalf("classify run: %v", err)
	}

	resp, ok := res.Response.(*response.ClassifyResponse)
	if !ok {
		t.Fatalf("response type = %T, want *response.ClassifyResponse", res.Response)
	}
	switch resp.Header().Outcome {
	case response.OutcomeBug, response.OutcomeFeature:
	default:
		t.Errorf("outcome = %q, want bug or feature", resp.Header().Outcome)
	}
	if !uuidV4Pattern.MatchString(res.SessionID) {
		t.Errorf("session id %q is not a v4 uuid", res.SessionID)
	}
}

// TestLive_HookSuppression is task 15(b): a project's own
// .claude/settings.json must not run under the argv claudeArgv builds.
//
// The hook event is SessionStart. claude --help's own built-in Hooks
// documentation (its Event/Matcher/Purpose table) lists SessionStart with
// matcher "-", firing unconditionally "When session starts" -- on every
// turn this runtime makes, first or resumed, whatever the model does or
// does not call. That makes it the one event guaranteed to fire during a
// run this short and this trivial: PreToolUse/PostToolUse only fire if the
// model calls a tool (not guaranteed here), and Stop fires at the very end
// (an errored or killed run might never reach it). SessionStart firing
// nowhere else to hide behind makes its absence the cleanest possible
// proof that --restricted really did ignore this project's settings file.
func TestLive_HookSuppression(t *testing.T) {
	skipUnlessLive(t)

	dir := fakeRepo(t)
	marker := filepath.Join(t.TempDir(), "hook-fired.marker")

	settingsDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	settings := fmt.Sprintf(
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch '%s'"}]}]}}`,
		marker,
	)
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatalf("write .claude/settings.json: %v", err)
	}

	const ticket = "Hook suppression smoke test\n\n" +
		"No tool calls or investigation are needed: reply immediately with " +
		"the bug outcome and the reason \"smoke test\"."

	req := RunRequest{
		Job:     response.JobClassify,
		Model:   liveModel(),
		Prompt:  classifyPrompt(t, ticket),
		Tools:   classifyTools(t),
		WorkDir: dir,
		Timeout: 3 * time.Minute,
	}

	if _, err := runLive(t, req); err != nil {
		t.Fatalf("hook suppression run: %v", err)
	}

	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("SessionStart hook fired: marker present or stat failed unexpectedly: %v", statErr)
	}
}

// TestLive_DontAskDenial is task 15(c): with Tools limited to read only (no
// Write), asking the model to write a file must complete with err == nil
// -- no permission prompt, no hang -- and the file must not exist:
// --permission-mode dontAsk denies without asking, and Write is absent
// from --tools in the first place, so there is nothing to approve.
func TestLive_DontAskDenial(t *testing.T) {
	skipUnlessLive(t)

	dir := fakeRepo(t)
	target := filepath.Join(dir, "should-not-exist.txt")

	ticket := fmt.Sprintf("Attempt a write\n\n"+
		"Use your Write tool to create the file %s with the content \"hello\". "+
		"Whether or not that succeeds, reply with the bug outcome and a "+
		"one-sentence reason noting whether the write tool was available.", target)

	req := RunRequest{
		Job:    response.JobClassify,
		Model:  liveModel(),
		Prompt: classifyPrompt(t, ticket),
		// testTools[:1] is claude_test.go's shared tool list narrowed to
		// its first entry, "read" -- reusing that literal rather than
		// spelling a new one out, so goconst does not see a fourth site
		// for the same repeated string.
		Tools:   testTools[:1],
		WorkDir: dir,
		Timeout: 3 * time.Minute,
	}

	if _, err := runLive(t, req); err != nil {
		t.Fatalf("dontAsk run: %v", err)
	}

	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("Write denial failed: target present or stat failed unexpectedly: %v", statErr)
	}
}
