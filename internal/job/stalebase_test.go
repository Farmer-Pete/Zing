// stalebase_test.go tests task 3: withStaleBaseNote and staleBaseSkip
// (stalebase.go), the shared shim that lets a fetch's fallback become one
// stale_base event on a tick's commit. No handler is wired to the shim
// yet (tasks 4 to 7 wire reviewing, judging, shipping, and building); this
// file drives withStaleBaseNote directly with a small run closure that
// fetches through the project's real orchestrator.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"zing/internal/gitfixture"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// staleBaseBreakOrigin makes ticketID's project fetch fail with reason
// no_origin: it makes sure refs/zing/base/<default> exists (seeding it
// from the local default branch when building never ran a fetch), then
// points origin at a directory that doesn't exist, adding the remote if
// the project has none yet.
func staleBaseBreakOrigin(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	proj, err := s.ProjectForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	baseRef := "refs/zing/base/" + pbFixtureDefaultBranch
	if _, err := gitfixture.Git(t.Context(), proj.LocalPath, "rev-parse", "--verify", baseRef); err != nil {
		if _, err := gitfixture.Git(t.Context(), proj.LocalPath, "update-ref", baseRef, "refs/heads/"+pbFixtureDefaultBranch); err != nil {
			t.Fatalf("update-ref %s: %v", baseRef, err)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing.git")
	if _, err := gitfixture.Git(t.Context(), proj.LocalPath, "remote", "get-url", "origin"); err == nil {
		if _, err := gitfixture.Git(t.Context(), proj.LocalPath, "remote", "set-url", "origin", missing); err != nil {
			t.Fatalf("remote set-url origin: %v", err)
		}
		return
	}
	if _, err := gitfixture.Git(t.Context(), proj.LocalPath, "remote", "add", "origin", missing); err != nil {
		t.Fatalf("remote add origin: %v", err)
	}
}

// withoutStaleBase returns msgs minus every message whose EventKind is
// stale_base, so an assertion written before this ticket can ignore the
// note a wrapped handler's tick now adds.
func withoutStaleBase(msgs []store.Message) []store.Message {
	out := make([]store.Message, 0, len(msgs))
	for i := range msgs {
		if msgs[i].EventKind != nil && *msgs[i].EventKind == store.EventKindStaleBase {
			continue
		}
		out = append(out, msgs[i])
	}
	return out
}

// staleBaseEventsFor reads ticketID's own stale_base events and returns
// the decoded payloads whose Step equals step.
func staleBaseEventsFor(t *testing.T, s *store.Store, ticketID int64, step string) []response.StaleBaseEvent {
	t.Helper()
	rows, err := s.Events(t.Context(), ticketID, store.EventKindStaleBase, store.EventFilter{})
	if err != nil {
		t.Fatalf("Events(stale_base): %v", err)
	}
	var out []response.StaleBaseEvent
	for i := range rows {
		var ev response.StaleBaseEvent
		if err := json.Unmarshal(rows[i].Payload, &ev); err != nil {
			t.Fatalf("decode stale_base payload %s: %v", rows[i].Payload, err)
		}
		if ev.Step == step {
			out = append(out, ev)
		}
	}
	return out
}

// staleBaseFetchRun is withStaleBaseNote's run argument for the tests
// below: it drives the ticket's real worktree and base fetch, exactly as
// a wrapped handler's own tick would, and returns a minimal but valid
// commit built from d's own claim fields.
func staleBaseFetchRun(ctx context.Context, tk store.Ticket, d Deps) (store.HandlerCommit, error) {
	proj := d.Projects[tk.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(ctx, tk.ID, tk.Title)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if _, err := proj.Orch.FetchBase(ctx, wt); err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{TicketID: tk.ID, Owner: d.Owner, Expires: d.Expires}, nil
}

// staleBaseRefSHA returns refs/zing/base/<default>'s own commit sha in
// ticketID's project checkout, for comparing against a stale_base event's
// own SHA field.
func staleBaseRefSHA(t *testing.T, s *store.Store, ticketID int64) string {
	t.Helper()
	proj, err := s.ProjectForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	out, err := gitfixture.Git(t.Context(), proj.LocalPath, "rev-parse", "refs/zing/base/"+pbFixtureDefaultBranch)
	if err != nil {
		t.Fatalf("rev-parse refs/zing/base/%s: %v", pbFixtureDefaultBranch, err)
	}
	return strings.TrimSpace(string(out))
}

// staleBaseApply applies commit directly through the store, skipping
// ValidateCommit's "the commit must do something" rule: staleBaseFetchRun
// returns a bare commit, which carries nothing at all once a tick's
// stale_base note is deduped, and such a commit still needs to clear the
// claim so the next tick can reclaim it.
func staleBaseApply(t *testing.T, s *store.Store, commit store.HandlerCommit) {
	t.Helper()
	applied, err := s.CommitHandlerResult(t.Context(), commit)
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}
}

// TestStaleBaseNoteOnce is this ticket's own done-when test (design demo):
// an unreachable origin, two reviewing ticks then one building tick, each
// through withStaleBaseNote directly (no handler wired yet). The reviewing
// ticks fall back to the same sha and post one note between them; the
// building tick, a different step at that same sha, posts a second.
func TestStaleBaseNoteOnce(t *testing.T) {
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	staleBaseBreakOrigin(t, s, ticket.ID)
	rt := runtime.NewFake(reviewScriptsFS(nil))

	tick := func(step string) store.HandlerCommit {
		deps := pbClaim(t, s, rt, ticket.ID)
		commit, err := withStaleBaseNote(t.Context(), ticket, deps, step, staleBaseFetchRun)
		if err != nil {
			t.Fatalf("withStaleBaseNote(%s): %v", step, err)
		}
		// A deduped tick's commit carries nothing beyond TicketID, Owner,
		// and Expires (staleBaseFetchRun's own bare return), so this
		// applies it straight through the store rather than through
		// pbApply: ValidateCommit's "must do something" rule is a
		// dispatcher-facing guard a synthetic run argument like this one
		// doesn't need to satisfy.
		staleBaseApply(t, s, commit)
		ticket = pbGetTicket(t, s, ticket.ID)
		return commit
	}

	tick(stateReviewing)
	tick(stateReviewing)

	wantSHA := staleBaseRefSHA(t, s, ticket.ID)

	n, err := s.CountEvents(t.Context(), ticket.ID, store.EventKindStaleBase, store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 1 {
		t.Fatalf("CountEvents after two reviewing ticks = %d, want 1", n)
	}
	reviewing := staleBaseEventsFor(t, s, ticket.ID, stateReviewing)
	if len(reviewing) != 1 {
		t.Fatalf("staleBaseEventsFor(reviewing) = %+v, want exactly one", reviewing)
	}
	if got := reviewing[0]; got.SHA != wantSHA || got.Reason != "no_origin" {
		t.Errorf("reviewing event = %+v, want sha %s reason no_origin", got, wantSHA)
	}

	tick(stateBuilding)

	n, err = s.CountEvents(t.Context(), ticket.ID, store.EventKindStaleBase, store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountEvents after the building tick = %d, want 2", n)
	}
	building := staleBaseEventsFor(t, s, ticket.ID, stateBuilding)
	if len(building) != 1 {
		t.Fatalf("staleBaseEventsFor(building) = %+v, want exactly one", building)
	}
	if got := building[0]; got.SHA != wantSHA || got.Reason != "no_origin" {
		t.Errorf("building event = %+v, want sha %s reason no_origin", got, wantSHA)
	}
}

// TestStaleBaseNoteSkippedWhenFetchWorks proves a tick whose fetch really
// reaches origin adds no stale_base event.
func TestStaleBaseNoteSkippedWhenFetchWorks(t *testing.T) {
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	basesyncAddOrigin(t, s, ticket)
	rt := runtime.NewFake(reviewScriptsFS(nil))
	deps := pbClaim(t, s, rt, ticket.ID)

	commit, err := withStaleBaseNote(t.Context(), ticket, deps, stateReviewing, staleBaseFetchRun)
	if err != nil {
		t.Fatalf("withStaleBaseNote: %v", err)
	}
	if kept := withoutStaleBase(commit.Messages); len(kept) != len(commit.Messages) {
		t.Errorf("commit.Messages = %+v, want no stale_base message", commit.Messages)
	}
}

// TestStaleBaseNoteDroppedOnTickError proves a tick that fetches, falls
// back, and then itself errs leaves no stale_base event behind, and that
// the fallback it took is gone: the next tick that falls back records it
// fresh.
func TestStaleBaseNoteDroppedOnTickError(t *testing.T) {
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	staleBaseBreakOrigin(t, s, ticket.ID)
	rt := runtime.NewFake(reviewScriptsFS(nil))
	deps := pbClaim(t, s, rt, ticket.ID)

	boom := errors.New("boom")
	run := func(ctx context.Context, tk store.Ticket, d Deps) (store.HandlerCommit, error) {
		if _, err := staleBaseFetchRun(ctx, tk, d); err != nil {
			return store.HandlerCommit{}, err
		}
		return store.HandlerCommit{}, boom
	}

	commit, err := withStaleBaseNote(t.Context(), ticket, deps, stateReviewing, run)
	if !errors.Is(err, boom) {
		t.Fatalf("withStaleBaseNote error = %v, want %v", err, boom)
	}
	if kept := withoutStaleBase(commit.Messages); len(kept) != len(commit.Messages) {
		t.Errorf("commit.Messages = %+v, want no stale_base message", commit.Messages)
	}

	proj := deps.Projects[ticket.ProjectID]
	if _, ok := proj.Orch.TakeBaseFallback(ticket.ID); ok {
		t.Error("TakeBaseFallback after an errored tick: ok = true, want false (the fallback was dropped)")
	}
}

// staleBaseRow builds a stale_base MessageRow carrying step, for
// TestStaleBaseSkip's "already noted" cases.
func staleBaseRow(t *testing.T, step string) store.MessageRow {
	t.Helper()
	msg, err := store.NewEvent(1, store.EventKindStaleBase, response.StaleBaseEvent{
		Step: step, Branch: "main", SHA: strings.Repeat("a", 40), Reason: "no_origin",
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	return store.MessageRow{Message: msg}
}

// TestReviewingTickNotesStaleBase proves reviewingHandler.Run's own shim
// (task 4) notes a stale base exactly like withStaleBaseNote does when
// driven directly: an unreachable origin, one reviewing tick through the
// real handler, then exactly one stale_base event for step reviewing with
// reason no_origin.
func TestReviewingTickNotesStaleBase(t *testing.T) {
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	staleBaseBreakOrigin(t, s, ticket.ID)
	rt := runtime.NewFake(reviewScriptsFS(nil))
	deps := pbClaim(t, s, rt, ticket.ID)

	ticket, _ = basesyncTick(t, s, deps, ticket, reviewingHandler{}, "reviewing")

	events := staleBaseEventsFor(t, s, ticket.ID, stateReviewing)
	if len(events) != 1 {
		t.Fatalf("staleBaseEventsFor(reviewing) = %+v, want exactly one", events)
	}
	if got := events[0]; got.Reason != "no_origin" {
		t.Errorf("reviewing event = %+v, want reason no_origin", got)
	}
}

// TestJudgingTickNotesStaleBase proves judgeHandler.Run's own shim (task 5)
// notes a stale base exactly like withStaleBaseNote does when driven
// directly: an unreachable origin, START, then one RUN tick through the
// real handler, then exactly one stale_base event for step judging with
// reason no_origin. The assertion covers both ticks, so it doesn't depend
// on whether START also fetched.
func TestJudgingTickNotesStaleBase(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	staleBaseBreakOrigin(t, s, ticket.ID)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))

	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	pbApply(t, s, ticket, commit)

	events := staleBaseEventsFor(t, s, ticket.ID, stateJudging)
	if len(events) != 1 {
		t.Fatalf("staleBaseEventsFor(judging) = %+v, want exactly one", events)
	}
	if got := events[0]; got.Reason != "no_origin" {
		t.Errorf("judging event = %+v, want reason no_origin", got)
	}
}

// TestShippingTickNotesStaleBase proves shipHandler.Run's own shim (task
// 6) notes a stale base exactly like withStaleBaseNote does when driven
// directly: shipTicketReady's bare origin, broken by staleBaseBreakOrigin,
// then one shipHandler tick with pr_url still unset, which reaches PUBLISH
// and fetches through Push. The resulting commit (an escalation or not) is
// applied either way, then exactly one stale_base event for step shipping
// with reason no_origin.
func TestShippingTickNotesStaleBase(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	staleBaseBreakOrigin(t, s, ticket.ID)

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)

	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pbApply(t, s, ticket, commit)

	events := staleBaseEventsFor(t, s, ticket.ID, stateShipping)
	if len(events) != 1 {
		t.Fatalf("staleBaseEventsFor(shipping) = %+v, want exactly one", events)
	}
	if got := events[0]; got.Reason != "no_origin" {
		t.Errorf("shipping event = %+v, want reason no_origin", got)
	}
}

// TestStaleBaseSkip is a table test over staleBaseSkip's pure decision
// (design shape): the sha and branch validity checks it repeats from the
// stale_base schema, and the already-noted check over a set of rows.
func TestStaleBaseSkip(t *testing.T) {
	sha40 := strings.Repeat("a", 40)
	sha64 := strings.Repeat("a", 64)
	reviewingRow := staleBaseRow(t, stateReviewing)
	badPayloadRow := store.MessageRow{Payload: json.RawMessage(`not json`)}

	tests := []struct {
		name string
		f    orchestrator.BaseFallback
		rows []store.MessageRow
		step string
		want string
	}{
		{"no rows", orchestrator.BaseFallback{Branch: "main", SHA: sha40, Reason: "no_origin"}, nil, stateReviewing, ""},
		{"same step already noted", orchestrator.BaseFallback{Branch: "main", SHA: sha40, Reason: "no_origin"}, []store.MessageRow{reviewingRow}, stateReviewing, staleBaseAlreadyNoted},
		{"different step", orchestrator.BaseFallback{Branch: "main", SHA: sha40, Reason: "no_origin"}, []store.MessageRow{reviewingRow}, stateBuilding, ""},
		{"undecodable row", orchestrator.BaseFallback{Branch: "main", SHA: sha40, Reason: "no_origin"}, []store.MessageRow{badPayloadRow}, stateReviewing, ""},
		{"64 hex sha", orchestrator.BaseFallback{Branch: "main", SHA: sha64, Reason: "no_origin"}, nil, stateReviewing, staleBasePayloadInvalid},
		{"empty branch", orchestrator.BaseFallback{Branch: "", SHA: sha40, Reason: "no_origin"}, nil, stateReviewing, staleBasePayloadInvalid},
		{"256 rune branch", orchestrator.BaseFallback{Branch: strings.Repeat("x", 256), SHA: sha40, Reason: "no_origin"}, nil, stateReviewing, staleBasePayloadInvalid},
		{"255 rune branch", orchestrator.BaseFallback{Branch: strings.Repeat("x", 255), SHA: sha40, Reason: "no_origin"}, nil, stateReviewing, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleBaseSkip(tc.f, tc.rows, tc.step); got != tc.want {
				t.Errorf("staleBaseSkip(%+v, rows=%d, step=%s) = %q, want %q", tc.f, len(tc.rows), tc.step, got, tc.want)
			}
		})
	}
}
