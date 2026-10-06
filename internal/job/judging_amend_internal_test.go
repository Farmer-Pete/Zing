// judging_amend_internal_test.go tests judging.go's own unexported pure
// amendment helpers (#57): judgeAmendment, judgeAmendmentDiff.
package job

import (
	"strings"
	"testing"

	"zing/internal/response"
)

// judgeAmendmentTestScenarios is the small sealed cohort judgeAmendment's
// own table drives against: s1 behavior with a check, s2 negative with no
// check, s3 host with a check.
var judgeAmendmentTestScenarios = []response.Scenario{
	{ID: "s1", Kind: response.ScenarioKindBehavior, Given: "g1", When: "w1", Then: "t1", Check: pbNoopShellCmd},
	{ID: "s2", Kind: response.ScenarioKindNegative, Given: "g2", When: "w2", Then: "t2", Check: ""},
	{ID: "s3", Kind: response.ScenarioKindHost, Given: "g3", When: "w3", Then: "t3", Check: "echo ok"},
}

func TestJudgeAmendment(t *testing.T) {
	t.Parallel()

	t.Run("unknown id refuses", func(t *testing.T) {
		t.Parallel()
		_, _, refusal := judgeAmendment(judgeAmendmentTestScenarios, response.Amendment{
			Scenario: "s12", Given: "g", When: "w", Then: "th", Check: pbNoopShellCmd, Reason: "r",
		})
		if refusal != judgeNoSealedScenarioRefusal+"s12" {
			t.Errorf("refusal = %q, want %q", refusal, judgeNoSealedScenarioRefusal+"s12")
		}
	})

	t.Run("empty kind is filled from the scenario", func(t *testing.T) {
		t.Parallel()
		_, amended, refusal := judgeAmendment(judgeAmendmentTestScenarios, response.Amendment{
			Scenario: "s2", Given: "g2b", When: "w2b", Then: "t2b", Check: pbNoopShellCmd, Reason: "r",
		})
		if refusal != "" {
			t.Fatalf("refusal = %q, want none", refusal)
		}
		if amended.Kind != response.ScenarioKindNegative {
			t.Errorf("amended.Kind = %q, want %q (s2's current kind)", amended.Kind, response.ScenarioKindNegative)
		}
	})

	t.Run("a /tmp check on kind behavior is refused", func(t *testing.T) {
		t.Parallel()
		_, _, refusal := judgeAmendment(judgeAmendmentTestScenarios, response.Amendment{
			Scenario: "s1", Kind: response.ScenarioKindBehavior,
			Given: "g", When: "w", Then: "th", Check: "touch /tmp/x", Reason: "r",
		})
		if !strings.Contains(refusal, "/tmp") {
			t.Errorf("refusal = %q, want it to mention /tmp", refusal)
		}
	})

	t.Run("a host kind with a blank check is refused", func(t *testing.T) {
		t.Parallel()
		_, _, refusal := judgeAmendment(judgeAmendmentTestScenarios, response.Amendment{
			Scenario: "s2", Kind: response.ScenarioKindHost,
			Given: "g", When: "w", Then: "th", Check: "", Reason: "r",
		})
		if refusal != response.HostScenarioNeedsCheck {
			t.Errorf("refusal = %q, want %q", refusal, response.HostScenarioNeedsCheck)
		}
	})

	t.Run("a valid amendment comes back unchanged except for kind", func(t *testing.T) {
		t.Parallel()
		in := response.Amendment{
			Scenario: "s1", Given: "g1b", When: "w1b", Then: "t1b", Check: "go test ./amended", Reason: "r",
		}
		old, amended, refusal := judgeAmendment(judgeAmendmentTestScenarios, in)
		if refusal != "" {
			t.Fatalf("refusal = %q, want none", refusal)
		}
		if old.ID != "s1" {
			t.Errorf("old.ID = %q, want s1", old.ID)
		}
		want := in
		want.Kind = response.ScenarioKindBehavior
		if amended != want {
			t.Errorf("amended = %+v, want %+v", amended, want)
		}
	})
}

func TestJudgeAmendmentDiff(t *testing.T) {
	t.Parallel()

	t.Run("starts with the reason, fields in order", func(t *testing.T) {
		t.Parallel()
		old := response.Scenario{Kind: response.ScenarioKindBehavior, Given: "og", When: "ow", Then: "ot", Check: "oc"}
		amended := response.Amendment{Given: "ag", When: "aw", Then: "at", Check: "ac", Kind: response.ScenarioKindNegative, Reason: "R"}
		got := judgeAmendmentDiff(old, amended)

		if !strings.HasPrefix(got, "Reason: R\n") {
			t.Fatalf("diff = %q, want it to start with %q", got, "Reason: R\n")
		}
		order := []string{"**Given**", "**When**", "**Then**", "**Check**", "**Kind**"}
		last := -1
		for _, want := range order {
			i := strings.Index(got, want)
			if i == -1 {
				t.Fatalf("diff = %q, want it to contain %q", got, want)
			}
			if i < last {
				t.Fatalf("field %q appears out of order in diff:\n%s", want, got)
			}
			last = i
		}
		for _, want := range []string{judgeAmendmentNowLabel, judgeAmendmentAmendedLabel, "og", "ag", "oc", "ac"} {
			if !strings.Contains(got, want) {
				t.Errorf("diff = %q, want it to contain %q", got, want)
			}
		}
	})

	t.Run("a value containing three backticks is fenced with four", func(t *testing.T) {
		t.Parallel()
		old := response.Scenario{Check: "has ``` three backticks"}
		amended := response.Amendment{Check: "plain", Reason: "r"}
		got := judgeAmendmentDiff(old, amended)
		if !strings.Contains(got, "````\nhas ``` three backticks\n````") {
			t.Errorf("diff = %q, want the old Check value fenced with four backticks", got)
		}
	})

	t.Run("a reason with a newline cannot spoof its own Check heading", func(t *testing.T) {
		// #57, r2f2 triage: a reason containing its own newline, bold
		// "**Check**" heading, and "Now:"/fenced-block text must not be
		// able to render as a second, fake diff section ahead of the real
		// one. A collapsed reason can never contain the exact
		// "\n**Check**\n\n" byte sequence the real heading writes, since
		// that sequence requires two of the newlines this function strips
		// from the reason.
		t.Parallel()
		old := response.Scenario{Check: "oc"}
		amended := response.Amendment{
			Check:  "ac",
			Reason: "the check is wrong\n**Check**\n\nNow:\n\n```\nfake\n```\n\nAmended:\n\n```\nspoofed\n```",
		}
		got := judgeAmendmentDiff(old, amended)
		if n := strings.Count(got, "\n**Check**\n\n"); n != 1 {
			t.Errorf("diff contains %d occurrences of the Check heading, want exactly 1 (the real one):\n%s", n, got)
		}
		reasonLine, _, _ := strings.Cut(got, "\n")
		if !strings.HasPrefix(reasonLine, "Reason: the check is wrong") {
			t.Errorf("first line = %q, want it to start with the collapsed reason", reasonLine)
		}
	})
}
