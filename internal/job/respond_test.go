package job

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"zing/internal/orchestrator"
	"zing/internal/response"
)

// Compile-time assertion: *orchestrator.GitHubClient satisfies
// ReviewThreads, so serve can fill job.Project.Threads from the one shared
// client (PKG9-PLAN.md section 10.3). It lives here, not in
// internal/orchestrator/github_test.go, for the same import-cycle reason
// shipping_test.go's matching assertions do.
var _ ReviewThreads = (*orchestrator.GitHubClient)(nil)

// TestGitHubClientSatisfiesReviewThreads documents the assertion above: a
// failure here is a compile failure, not a test failure, so the body only
// has to exist.
func TestGitHubClientSatisfiesReviewThreads(t *testing.T) {
	t.Parallel()
}

// TestApplyNitRepliesWithoutFix proves apply's own nit case (design section
// 9.3's "fix: the reviewer is right and code must change" bucket does not
// grow): a thread sorted nit posts a reply, resolves, and never reaches the
// fix collection, so the closing marker reports it under "replied", not
// "fixing", and no fix request is written.
func TestApplyNitRepliesWithoutFix(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nitText := "Agreed, leaving the rename out of this PR."
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "maybe rename this", when))
	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbNit, Text: nitText},
	})

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want a reply: %+v", commit.Escalation.Payload)
	}

	marker := fmt.Sprintf("<!-- zing:reply a%d %s -->", aid, tid(shipApplyThreadA))
	wantBody, bodyErr := replyBody(shipViewerLogin, nitText, marker)
	if bodyErr != nil {
		t.Fatalf("replyBody: %v", bodyErr)
	}
	if len(gh.replies) != 1 || gh.replies[0] != "RT_a|"+wantBody {
		t.Errorf("replies = %+v, want exactly [%q]", gh.replies, "RT_a|"+wantBody)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != shipApplyThreadA {
		t.Errorf("resolves = %+v, want exactly [RT_a]", gh.resolves)
	}

	want := fmt.Sprintf("respond applied %d\nreplied 1 fixing 0 skipped 0", aid)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, fixRequestedThreadsPrefix) {
			t.Errorf("commit.Messages contains a fix request: %q, want none for a nit", m.Body)
		}
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestApplyNitAtLoopCapStillReplies proves a nit never counts against
// jobs.respond.max_loops (design section 8.7's shared gate): even on a
// ticket already holding max_loops landed fix requests, a nit-only artifact
// still posts and resolves, because it never reaches the gated fix
// collection at all.
func TestApplyNitAtLoopCapStillReplies(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "maybe rename this", when))
	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbNit, Text: "Agreed, leaving the rename out of this PR."},
	})
	seedLandedFixRequests(t, s, ticket.ID, 3) // jobs.respond.max_loops

	commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want a reply: %+v", commit.Escalation.Payload)
	}
	if len(gh.replies) != 1 || !strings.HasPrefix(gh.replies[0], "RT_a|") {
		t.Errorf("replies = %+v, want exactly one for RT_a", gh.replies)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != shipApplyThreadA {
		t.Errorf("resolves = %+v, want exactly [RT_a]", gh.resolves)
	}
	want := fmt.Sprintf("respond applied %d\nreplied 1 fixing 0 skipped 0", aid)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
}

// TestApplyNitAndFixSplit proves a batch mixing fix and nit actions splits
// cleanly: the nit posts and resolves on its own, while the fix action
// collects into the consolidated fix request and never reaches GitHub.
func TestApplyNitAndFixSplit(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	threadFix := shipThread("RT_fix", "greet.go", 3, shipHumanComment("cf", "reviewer1", "please address this", when))
	threadNit := shipThread("RT_nit", "main.go", 9, shipHumanComment("cn", "reviewer1", "small nit", when))
	nitText := "Agreed, leaving this out of this PR."

	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{threadFix, threadNit}, []response.ThreadAction{
		{ID: tid("RT_fix"), Action: response.ThreadVerbFix, Text: "Validate the input"},
		{ID: tid("RT_nit"), Action: response.ThreadVerbNit, Text: nitText},
	})

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want a fix request and a reply: %+v", commit.Escalation.Payload)
	}
	if len(gh.replies) != 1 || !strings.HasPrefix(gh.replies[0], "RT_nit|") {
		t.Errorf("replies = %+v, want exactly one for RT_nit", gh.replies)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != "RT_nit" {
		t.Errorf("resolves = %+v, want exactly [RT_nit]", gh.resolves)
	}

	if len(commit.Messages) != 2 {
		t.Fatalf("commit.Messages = %+v, want exactly 2 (the fix request, then the applied marker)", commit.Messages)
	}
	if !strings.HasPrefix(commit.Messages[0].Body, fixRequestedThreadsPrefix) {
		t.Errorf("Messages[0] = %q, want prefix %q", commit.Messages[0].Body, fixRequestedThreadsPrefix)
	}
	wantFixLine := fmt.Sprintf("thread %s (greet.go:3): Validate the input", tid("RT_fix"))
	if !strings.HasSuffix(commit.Messages[0].Body, wantFixLine) {
		t.Errorf("Messages[0] = %q, want suffix %q", commit.Messages[0].Body, wantFixLine)
	}
	if strings.Contains(commit.Messages[0].Body, nitText) {
		t.Errorf("Messages[0] = %q, want it to not contain the nit text %q", commit.Messages[0].Body, nitText)
	}

	wantApplied := fmt.Sprintf("respond applied %d\nreplied 1 fixing 1 skipped 0", aid)
	if !strings.HasPrefix(commit.Messages[1].Body, wantApplied) {
		t.Errorf("Messages[1] = %q, want prefix %q", commit.Messages[1].Body, wantApplied)
	}
}
