// stalebase_external_test.go tests task 7: buildingHandler.Run's own
// withStaleBaseNote shim (building.go), from package job_test since
// building's richer fixtures (buildTicketInBuilding, claimForBuild, apply,
// getTicket) live there, not in package job alongside stalebase_test.go.
// It carries its own copy of stalebase_test.go's staleBaseBreakOrigin,
// because package job_test can't reach package job's unexported test
// helpers. No building or escalation test's own assertion broke once
// buildingHandler.Run started posting its own stale_base note (every one
// of them either doesn't count commit.Messages exactly or runs against a
// project with a working origin already), so this file carries no
// withoutStaleBase copy: Go's unused-function check would refuse one with
// no caller.
package job_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/store"
)

// staleBaseBreakOrigin is stalebase_test.go's own helper (package job),
// copied here for package job_test: it makes ticketID's project fetch fail
// with reason no_origin, seeding refs/zing/base/<default> first when
// nothing has fetched it yet.
func staleBaseBreakOrigin(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	proj, err := s.ProjectForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	baseRef := "refs/zing/base/" + testFixtureDefaultBranch
	if _, err := gitfixture.Git(t.Context(), proj.LocalPath, "rev-parse", "--verify", baseRef); err != nil {
		if _, err := gitfixture.Git(t.Context(), proj.LocalPath, "update-ref", baseRef, "refs/heads/"+testFixtureDefaultBranch); err != nil {
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

// TestBuildingTickNotesStaleBase proves buildingHandler.Run's own shim
// (task 7) notes a stale base exactly like withStaleBaseNote does when
// driven directly: buildTicketInBuilding leaves no worktree yet, an
// unreachable origin breaks the fetch EnsureWorktree makes, and one
// building tick through the real handler adds exactly one stale_base event
// for step building with reason no_origin.
func TestBuildingTickNotesStaleBase(t *testing.T) {
	t.Parallel()
	s, rt, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, rt, ticketID)

	proj, ok := deps.Projects[ticket.ProjectID]
	if !ok {
		t.Fatalf("no git-backed project wired for ticket %d", ticket.ID)
	}
	if _, err := proj.Orch.ExistingWorktree(t.Context(), ticket.ID); !errors.Is(err, orchestrator.ErrNoWorktree) {
		t.Fatalf("ExistingWorktree before the tick: err = %v, want errors.Is(err, orchestrator.ErrNoWorktree)", err)
	}

	staleBaseBreakOrigin(t, s, ticket.ID)

	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	rows, err := s.Events(t.Context(), ticket.ID, store.EventKindStaleBase, store.EventFilter{})
	if err != nil {
		t.Fatalf("Events(stale_base): %v", err)
	}
	var building []response.StaleBaseEvent
	for i := range rows {
		var ev response.StaleBaseEvent
		if err := json.Unmarshal(rows[i].Payload, &ev); err != nil {
			t.Fatalf("decode stale_base payload %s: %v", rows[i].Payload, err)
		}
		if ev.Step == testStateBuilding {
			building = append(building, ev)
		}
	}
	if len(building) != 1 {
		t.Fatalf("stale_base events for building = %+v, want exactly one", building)
	}
	if got := building[0]; got.Reason != "no_origin" {
		t.Errorf("building event = %+v, want reason no_origin", got)
	}
}
