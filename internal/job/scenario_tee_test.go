package job

import (
	"testing"

	"zing/internal/response"
)

// TestCheckScenarioShape_RejectsTeeThenRead is a regression test for #248: a
// sealed check shaped like CMD | tee FILE | grep -q A, followed by a later
// read of FILE, breaks because grep -q exits on its first match, tee then
// gets SIGPIPE and stops writing FILE, and the later read sees a cut-off
// log. The check fails even when every test passes. Seen as s6 and host
// s14 on #248; s14's own fix run changed no code and escalated. Each
// offending check is refused at planning time, pointing at writing the log
// first, then grepping it.
func TestCheckScenarioShape_RejectsTeeThenRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		kind  response.ScenarioKind
		check string
	}{
		{"s14_host", response.ScenarioKindHost, `go test -count=1 -run TestJudgeClaude -v ./internal/orchestrator/ 2>&1 | tee "$TMPDIR/s14.log" | grep -q -- '--- PASS: TestJudgeClaude' && ! grep -q -- '--- FAIL' "$TMPDIR/s14.log"`},
		{"s6_behavior", response.ScenarioKindBehavior, `go test ./x 2>&1 | tee "$TMPDIR/s6.log" | grep -q ok; grep -q PASS "$TMPDIR/s6.log"`},
		{"head", response.ScenarioKindBehavior, `go test ./x | tee "$TMPDIR/out" | head -1 && grep -q ok "$TMPDIR/out"`},
		{"grep_l", response.ScenarioKindBehavior, `go test ./x | tee "$TMPDIR/out" | grep -l ok && grep -q PASS "$TMPDIR/out"`},
		{"grep_count", response.ScenarioKindBehavior, `go test ./x | tee "$TMPDIR/out" | grep -c ok || grep -q FAIL "$TMPDIR/out"`},
		{"append_flag", response.ScenarioKindBehavior, `go test ./x | tee -a "$TMPDIR/out" | grep -q ok && grep -q PASS "$TMPDIR/out"`},
		{"unquoted_file", response.ScenarioKindBehavior, `go test ./x | tee $TMPDIR/out | grep -q ok && grep -q PASS $TMPDIR/out`},
		{"newline_list", response.ScenarioKindBehavior, "go test ./x | tee \"$TMPDIR/out\" | grep -q ok\ngrep -q PASS \"$TMPDIR/out\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scenarios := []response.Scenario{
				{ID: "s1", Kind: tc.kind, Then: "the tests pass", Check: tc.check},
				cleanScenario,
			}
			errs := checkScenarioShape(scenarios)
			if len(errs) != 1 {
				t.Fatalf("checkScenarioShape(%q) = %+v, want exactly one error", tc.check, errs)
			}
			if errs[0].Path != "scenarios/scenario[0]/check" {
				t.Errorf("checkScenarioShape(%q) error Path = %q, want %q", tc.check, errs[0].Path, "scenarios/scenario[0]/check")
			}
			if errs[0].Msg != teeReadCheckMsg {
				t.Errorf("checkScenarioShape(%q) error Msg = %q, want %q", tc.check, errs[0].Msg, teeReadCheckMsg)
			}
		})
	}
}

// TestCheckScenarioShape_AllowsLogThenGrep checks that the tee rule only
// matches a check that pipes tee's output into another command and reads
// the tee'd file later in the same check: the owner's rewrite, a tee never
// reread, a tee followed by a semicolon or an or-list instead of a pipe, a
// tee piped to /dev/null, "tee" appearing inside another word, a check that
// reads a different file later, and tee operands that the dash and
// redirect filters must skip all pass.
func TestCheckScenarioShape_AllowsLogThenGrep(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		check string
	}{
		{"s14_rewritten", `go test -count=1 -run TestJudgeClaude -v ./internal/orchestrator/ > "$TMPDIR/s14.log" 2>&1; grep -q -- '--- PASS: TestJudgeClaude' "$TMPDIR/s14.log" && ! grep -q -- '--- FAIL' "$TMPDIR/s14.log"`},
		{"tee_never_reread", `go test ./x 2>&1 | tee "$TMPDIR/out" | grep -q ok`},
		{"tee_no_pipe", `go test ./x | tee "$TMPDIR/out"; grep -q ok "$TMPDIR/out"`},
		{"tee_or_list", `go test ./x | tee "$TMPDIR/out" || true; grep -q ok "$TMPDIR/out"`},
		{"tee_to_devnull", `go test ./x | tee "$TMPDIR/out" > /dev/null && grep -q ok "$TMPDIR/out"`},
		{"tee_inside_word", `go test ./x | grep -q guarantee && grep -q ok "$TMPDIR/out"`},
		{"other_file_read_later", `go test ./x | tee "$TMPDIR/a" | grep -q ok && grep -q ok "$TMPDIR/b"`},
		{"dash_operand_not_a_file", `go test ./x | tee -a "$TMPDIR/out" | grep -q ok && grep -a ok "$TMPDIR/other"`},
		{"redirect_operand_not_a_file", `go test ./x | tee "$TMPDIR/out" 2>/dev/null | grep -q ok && grep -q x 2>/dev/null "$TMPDIR/b"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sc := response.Scenario{ID: "s1", Kind: response.ScenarioKindBehavior, Then: "the tests pass", Check: tc.check}
			for _, err := range checkScenarioRules(0, sc) {
				if err.Msg == teeReadCheckMsg {
					t.Fatalf("checkScenarioRules(%q) flagged tee into a reader, want it allowed", tc.check)
				}
			}
		})
	}
}
