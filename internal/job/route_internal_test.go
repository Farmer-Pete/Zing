package job

// route_internal_test.go tests route.go's own unexported functions that no
// seam in planning_test.go (package job_test) can reach.

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
	"zing/internal/store"
)

// TestPostRunFailedWhatFor covers postRunFailedWhatFor's own table (design
// section 4.1): the four origins runAndRoute already threads through
// postRunFailure, the three origins Package 8 adds ahead of their own
// callers, the three origins Package 9 adds the same way, and an unknown
// origin's fallback sentence.
func TestPostRunFailedWhatFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		origin response.EscalationOrigin
		want   string
	}{
		{response.EscalationOriginClassify, "classifying the ticket"},
		{response.EscalationOriginPlanningFirst, "storing or checking the plan"},
		{response.EscalationOriginPlanningResume, "storing or checking the plan"},
		{response.EscalationOriginPlanreview, "storing the plan review"},
		{response.EscalationOriginBuild, "storing or checking the build result"},
		{response.EscalationOriginFix, "storing or checking the build result"},
		{response.EscalationOriginPerimeter, "storing the perimeter description"},
		{response.EscalationOriginReview, "storing the review findings"},
		{response.EscalationOriginJudge, "storing or checking the verdicts"},
		{response.EscalationOriginRespond, "storing the thread actions"},
		{response.EscalationOriginSeal, "storing or checking the agent's result"},
	}
	for _, tt := range tests {
		t.Run(string(tt.origin), func(t *testing.T) {
			t.Parallel()
			if got := postRunFailedWhatFor(tt.origin); got != tt.want {
				t.Errorf("postRunFailedWhatFor(%s) = %q, want %q", tt.origin, got, tt.want)
			}
		})
	}
}

// TestSchemaInvalidEscalation proves SchemaInvalidEscalation's mapping and
// copying rules (design section "job"): each of the six states that can
// reach it maps to its own origin, the failed commit's runs with an id
// above 0 terminalize as error while its own exit code and agent seconds
// pass through unchanged, Session/ResolveQuestions are carried the same
// way postRunFailure carries them (owner decision Q5), a Sessions entry
// with no ID is dropped (an entry's own ID is what every further session
// write requires), and a state with no mapped origin (done) returns ok
// false so the dispatcher's own caller falls through to fail-closed.
func TestSchemaInvalidEscalation(t *testing.T) {
	t.Parallel()

	t.Run("origin per state", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			state      string
			wantOrigin response.EscalationOrigin
		}{
			{stateQueued, response.EscalationOriginClassify},
			{statePlanning, response.EscalationOriginPlanningResume},
			{stateBuilding, response.EscalationOriginBuild},
			{stateReviewing, response.EscalationOriginReview},
			{stateJudging, response.EscalationOriginJudge},
			{stateShipping, response.EscalationOriginShipping},
		}
		for _, tt := range tests {
			t.Run(tt.state, func(t *testing.T) {
				t.Parallel()
				ticket := store.Ticket{ID: 1, State: tt.state}
				failed := store.HandlerCommit{TicketID: 1}
				c, ok := SchemaInvalidEscalation(ticket, failed, errors.New("payload does not match schema question: /options: got null, want array"))
				if !ok {
					t.Fatalf("SchemaInvalidEscalation(%s) ok = false, want true", tt.state)
				}
				if c.Escalation == nil {
					t.Fatalf("SchemaInvalidEscalation(%s): Escalation = nil", tt.state)
				}
				if got := c.Escalation.Payload.Origin; got != string(tt.wantOrigin) {
					t.Errorf("SchemaInvalidEscalation(%s): origin = %q, want %q", tt.state, got, tt.wantOrigin)
				}
			})
		}
	})

	t.Run("building: full shape", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{ID: 1, State: stateBuilding}
		exitCode := 3
		agentSeconds := 42
		sessionID := int64(55)
		ext := "ext-55"
		keptID := int64(90)
		keptExt := "ext-90"
		droppedExt := "ext-dropped"
		resolveIDs := []int64{11, 12}
		failErr := errors.New("payload does not match schema question: /options: got null, want array")

		failed := store.HandlerCommit{
			TicketID: 1, Owner: "owner-1", Expires: time.Now().Add(time.Hour),
			Runs:             []store.Run{{ID: 7, Turn: 2, ExitCode: &exitCode, AgentSeconds: &agentSeconds}, {ID: 0, Turn: 1}},
			Session:          &store.SessionUpsert{ID: &sessionID, ExternalID: &ext},
			Sessions:         []store.SessionUpsert{{ID: &keptID, ExternalID: &keptExt}, {ExternalID: &droppedExt}},
			ResolveQuestions: resolveIDs,
			ResolveAll:       false,
		}

		c, ok := SchemaInvalidEscalation(ticket, failed, failErr)
		if !ok {
			t.Fatal("SchemaInvalidEscalation ok = false, want true")
		}

		if c.Owner != failed.Owner {
			t.Errorf("Owner = %q, want %q", c.Owner, failed.Owner)
		}
		if !c.Expires.Equal(failed.Expires) {
			t.Errorf("Expires = %v, want %v", c.Expires, failed.Expires)
		}
		if c.Escalation == nil {
			t.Fatal("Escalation = nil")
		}
		if c.Escalation.RunID != nil {
			t.Errorf("Escalation.RunID = %v, want nil", *c.Escalation.RunID)
		}
		if c.Escalation.Payload.Code != string(response.EscalationCodePostRunFailed) {
			t.Errorf("Payload.Code = %q, want %q", c.Escalation.Payload.Code, response.EscalationCodePostRunFailed)
		}
		if c.Waiting == nil || *c.Waiting != waitingFlagQuestions {
			t.Errorf("Waiting = %v, want %q", c.Waiting, waitingFlagQuestions)
		}

		if len(c.Runs) != 1 {
			t.Fatalf("Runs = %+v, want exactly 1", c.Runs)
		}
		if c.Runs[0].ID != 7 {
			t.Errorf("Runs[0].ID = %d, want 7", c.Runs[0].ID)
		}
		if c.Runs[0].Outcome == nil || *c.Runs[0].Outcome != string(response.OutcomeError) {
			t.Errorf("Runs[0].Outcome = %v, want %q", c.Runs[0].Outcome, response.OutcomeError)
		}
		if c.Runs[0].ExitCode == nil || *c.Runs[0].ExitCode != exitCode {
			t.Errorf("Runs[0].ExitCode = %v, want %d", c.Runs[0].ExitCode, exitCode)
		}
		if c.Runs[0].AgentSeconds == nil || *c.Runs[0].AgentSeconds != agentSeconds {
			t.Errorf("Runs[0].AgentSeconds = %v, want %d", c.Runs[0].AgentSeconds, agentSeconds)
		}

		tried := c.Escalation.Payload.Tried
		if !strings.HasPrefix(tried, failErr.Error()) {
			t.Errorf("Tried = %q, want it to start with %q", tried, failErr.Error())
		}
		if !strings.HasSuffix(tried, "run ids: 7") {
			t.Errorf("Tried = %q, want it to end with %q", tried, "run ids: 7")
		}

		if c.Session == nil || c.Session.ID == nil || *c.Session.ID != sessionID || c.Session.ExternalID == nil || *c.Session.ExternalID != ext {
			t.Errorf("Session = %v, want ID %d and ExternalID %q", c.Session, sessionID, ext)
		}
		if len(c.Sessions) != 1 || c.Sessions[0].ID == nil || *c.Sessions[0].ID != keptID || c.Sessions[0].ExternalID == nil || *c.Sessions[0].ExternalID != keptExt {
			t.Errorf("Sessions = %+v, want exactly the one entry with ID %d and ExternalID %q", c.Sessions, keptID, keptExt)
		}
		if !slices.Equal(c.ResolveQuestions, resolveIDs) {
			t.Errorf("ResolveQuestions = %v, want %v", c.ResolveQuestions, resolveIDs)
		}
	})

	t.Run("no runs gives run ids none", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{ID: 1, State: stateBuilding}
		c, ok := SchemaInvalidEscalation(ticket, store.HandlerCommit{TicketID: 1}, errors.New("boom"))
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if !strings.HasSuffix(c.Escalation.Payload.Tried, "run ids: none") {
			t.Errorf("Tried = %q, want it to end with %q", c.Escalation.Payload.Tried, "run ids: none")
		}
	})

	t.Run("session with nil ID is dropped", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{ID: 1, State: stateBuilding}
		ext := "ext-no-id"
		failed := store.HandlerCommit{TicketID: 1, Session: &store.SessionUpsert{ExternalID: &ext}}
		c, ok := SchemaInvalidEscalation(ticket, failed, errors.New("boom"))
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if c.Session != nil {
			t.Errorf("Session = %+v, want nil (the failed commit's own Session had no ID)", c.Session)
		}
	})

	t.Run("state with no mapped origin returns ok false", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{ID: 1, State: stateDone}
		_, ok := SchemaInvalidEscalation(ticket, store.HandlerCommit{TicketID: 1}, errors.New("boom"))
		if ok {
			t.Error("ok = true, want false for state done")
		}
	})
}
