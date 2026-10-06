package job

import (
	"testing"

	"zing/internal/response"
	"zing/internal/store"
)

// TestCapResumesEscalationNamesJob is task 7's named test: capResumesEscalation
// must name the exhausted session's own job in both What and Why, for every
// job that calls it, not just planning (design D17).
func TestCapResumesEscalationNamesJob(t *testing.T) {
	names := []string{jobPlanningName, jobBuildName, jobReviewName, jobJudgeName, jobRespondName}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			commit := capResumesEscalation(store.Ticket{ID: 1}, Deps{}, name, 7)
			if commit.Escalation == nil {
				t.Fatalf("capResumesEscalation(%q): Escalation is nil", name)
			}
			wantWhat := "raise machine.toml's " + name + " max_resumes, or abandon"
			wantWhy := "the " + name + " session has resumed the maximum number of times machine.toml allows"
			if commit.Escalation.Payload.What != wantWhat {
				t.Errorf("capResumesEscalation(%q): What = %q, want %q", name, commit.Escalation.Payload.What, wantWhat)
			}
			if commit.Escalation.Payload.Why != wantWhy {
				t.Errorf("capResumesEscalation(%q): Why = %q, want %q", name, commit.Escalation.Payload.Why, wantWhy)
			}
			wantBody := string(response.EscalationCodeResumesExhausted) + ": " + wantWhat
			if commit.Escalation.Body != wantBody {
				t.Errorf("capResumesEscalation(%q): Body = %q, want %q", name, commit.Escalation.Body, wantBody)
			}
			if commit.Escalation.Payload.SessionID == nil || *commit.Escalation.Payload.SessionID != 7 {
				t.Errorf("capResumesEscalation(%q): SessionID = %v, want 7", name, commit.Escalation.Payload.SessionID)
			}
		})
	}
}
