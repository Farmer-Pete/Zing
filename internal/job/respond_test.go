package job

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/store"
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

// TestThreadReadyCycles proves threadReadyCycles' own scan (design section
// 8.5 row 3's shape table): a "pr draft" marker counts a finished cycle
// only when a "respond batch N started" line falls between it and the next
// "pr ready" marker; anything else -- no batch, a batch with no closing
// ready, a skipped or stale line in place of a started one, or a second
// draft before the ready -- counts nothing.
func TestThreadReadyCycles(t *testing.T) {
	t.Parallel()
	shaA := strings.Repeat("a", 40)
	shaC := strings.Repeat("c", 40)
	shaX := strings.Repeat("e", 40)

	tests := []struct {
		name    string
		markers []store.MessageRow
		want    int
	}{
		{name: "empty", markers: nil, want: 0},
		{
			name: "worked example",
			markers: []store.MessageRow{
				{ID: 10, Body: prReadyPrefix + "A"},
				{ID: 11, Body: prDraftPrefix + "A"},
				{ID: 12, Body: fmt.Sprintf("respond batch 1 started sha %s after run 4", shaA)},
				{ID: 15, Body: prReadyPrefix + "B"},
				{ID: 16, Body: prDraftPrefix + "B"},
				{ID: 18, Body: prReadyPrefix + "C"},
				{ID: 19, Body: prDraftPrefix + "C"},
				{ID: 20, Body: fmt.Sprintf("respond batch 2 started sha %s after run 9", shaC)},
				{ID: 25, Body: prReadyPrefix + "D"},
			},
			want: 2,
		},
		{
			name: "draft then ready with no batch",
			markers: []store.MessageRow{
				{ID: 1, Body: prDraftPrefix + "X"},
				{ID: 2, Body: prReadyPrefix + "X"},
			},
			want: 0,
		},
		{
			name: "draft then a started batch with no ready",
			markers: []store.MessageRow{
				{ID: 1, Body: prDraftPrefix + "X"},
				{ID: 2, Body: fmt.Sprintf("respond batch 1 started sha %s after run 0", shaX)},
			},
			want: 0,
		},
		{
			name: "draft, a skipped batch, a stale batch, then ready",
			markers: []store.MessageRow{
				{ID: 1, Body: prDraftPrefix + "X"},
				{ID: 2, Body: fmt.Sprintf("respond batch %d skipped", 1)},
				{ID: 3, Body: "respond batch 2 stale"},
				{ID: 4, Body: prReadyPrefix + "X"},
			},
			want: 0,
		},
		{
			name: "draft, started batch, a second draft, then ready",
			markers: []store.MessageRow{
				{ID: 1, Body: prDraftPrefix + "X"},
				{ID: 2, Body: fmt.Sprintf("respond batch 1 started sha %s after run 0", shaX)},
				{ID: 3, Body: prDraftPrefix + "Y"},
				{ID: 4, Body: prReadyPrefix + "Y"},
			},
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := threadReadyCycles(tc.markers); got != tc.want {
				t.Errorf("threadReadyCycles(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// seedReadyCycles writes n finished thread-caused ready, draft, ready
// cycles directly, the shape threadReadyCycles (respond.go) counts: an
// initial "pr ready" marker, then per cycle k (1 to n) a "pr draft"
// marker, a "respond batch k started" line, a "respond batch k stale"
// line, and "pr ready" again -- standing in for n real poll/respond
// cycles, all in one handler commit (seedRespondSkippedBatches' own
// shape).
func seedReadyCycles(t *testing.T, s *store.Store, ticketID int64, sha string, n int) {
	t.Helper()
	owner := "seed-ready-cycles"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedReadyCycles: claim: claimed=%v err=%v", claimed, err)
	}
	msgs := []store.Message{{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: prReadyPrefix + sha}}
	for k := 1; k <= n; k++ {
		msgs = append(msgs,
			store.Message{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: prDraftPrefix + sha},
			store.Message{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: fmt.Sprintf("respond batch %d started sha %s after run 0", k, sha)},
			store.Message{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: fmt.Sprintf("respond batch %d stale", k)},
			store.Message{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: prReadyPrefix + sha},
		)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires, Messages: msgs,
	})
	if err != nil || !applied {
		t.Fatalf("seedReadyCycles: commit: applied=%v err=%v", applied, err)
	}
}

// shipNodeCapped is the pull request node id every ready-cycle-cap test
// below configures gh.prState and asserts convertToDraftCalls with
// (goconst: repeated across the three cap tests).
const shipNodeCapped = "PR_node_capped"

// TestPollReadyCycleCapKeepsPRReady proves the cap once per thread author: a
// bot's nit and a human's comment both count as an unresolved actionable
// thread (design non-goal: every thread-caused cycle counts, not only a
// bot's).
func TestPollReadyCycleCapKeepsPRReady(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	for _, author := range []string{"coderabbitai", "reviewer1"} {
		t.Run(author, func(t *testing.T) {
			t.Parallel()
			s, ticket, gh, tr := shipPublished(t)
			local := shipHeadSHA(t, s, ticket)
			runs, required := shipGreenCI()
			gh.runs, gh.required = runs, required
			gh.prState = orchestrator.PRState{Draft: false, HeadSHA: local, BaseRef: pbFixtureDefaultBranch, NodeID: shipNodeCapped}
			seedReadyCycles(t, s, ticket.ID, local, readyCycleCap)
			gh.threads = []orchestrator.Thread{
				shipThread("RT_capped", "greet.go", 3, shipHumanComment("c1", author, "nit: rename this", time.Now())),
			}

			commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(gh.convertToDraftCalls) != 0 {
				t.Errorf("convertToDraftCalls = %+v, want none: the cap keeps the pull request ready", gh.convertToDraftCalls)
			}
			wantPrefix := "respond batch 3 started sha " + local + " after run"
			found := false
			for i := range commit.Messages {
				if strings.HasPrefix(commit.Messages[i].Body, wantPrefix) {
					found = true
				}
			}
			if !found {
				t.Errorf("commit.Messages = %+v, want a message with prefix %q", commit.Messages, wantPrefix)
			}
		})
	}
}

// TestPollBelowReadyCycleCapFlipsToDraft proves the cap holds no effect
// below readyCycleCap: one finished cycle, one new unresolved thread, and
// row 3 flips the pull request to draft exactly as it does with no cap at
// all (TestDraftWhenLoopReopens, shipping_test.go).
func TestPollBelowReadyCycleCapFlipsToDraft(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: false, HeadSHA: local, BaseRef: pbFixtureDefaultBranch, NodeID: shipNodeCapped}
	seedReadyCycles(t, s, ticket.ID, local, readyCycleCap-1)
	gh.threads = []orchestrator.Thread{
		shipThread("RT_below_cap", "greet.go", 3, shipHumanComment("c1", "reviewer1", "nit: rename this", time.Now())),
	}

	commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.convertToDraftCalls) != 1 || gh.convertToDraftCalls[0] != shipNodeCapped {
		t.Fatalf("convertToDraftCalls = %+v, want exactly [%q]", gh.convertToDraftCalls, shipNodeCapped)
	}
	want := prDraftPrefix + local
	found := false
	for i := range commit.Messages {
		if commit.Messages[i].Body == want {
			found = true
		}
	}
	if !found {
		t.Errorf("commit.Messages = %+v, want %q", commit.Messages, want)
	}
}

// TestPollReadyCycleCapStillFlipsOnCIFailure proves design section 8.5 row
// 3's own closing rule: whatever the cap already counted, a failed required
// check still converts a ready pull request to draft (skipThreadDraftFlip
// must not skip the flip just because the cap is reached).
func TestPollReadyCycleCapStillFlipsOnCIFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipFailedCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: false, HeadSHA: local, BaseRef: pbFixtureDefaultBranch, NodeID: shipNodeCapped}
	gh.logTail = func(context.Context, string, string, int64, int) (string, error) { return shipCILogTailText, nil }
	seedReadyCycles(t, s, ticket.ID, local, readyCycleCap)
	seedSpentFlakyCheckRerun(t, s, ticket.ID, local)

	_, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.convertToDraftCalls) != 1 || gh.convertToDraftCalls[0] != shipNodeCapped {
		t.Fatalf("convertToDraftCalls = %+v, want exactly [%q]", gh.convertToDraftCalls, shipNodeCapped)
	}
}
