package store

import (
	"reflect"
	"testing"

	"zing/internal/response"
)

// TestEscalationOptionsFor unit-tests escalationOptionsFor directly (#47
// item 2, plan "Tests to write first" item 5): post-seal (any ticket state
// but "planning") always offers Retry and Abandon only, recommending Retry;
// within planning, split_unsupported and nothing_to_do_with_true_claims
// recommend "b" (back to planning cannot run post-seal, so those two codes
// only ever reach this with ticketState == "planning"), every other code
// recommends "a". No DB: escalationOptionsFor is a pure function.
func TestEscalationOptionsFor(t *testing.T) {
	t.Parallel()

	wantPostSeal := []response.Option{
		{Key: "a", Text: escalationOptionRetry},
		{Key: "c", Text: escalationOptionAbandon},
	}
	wantPlanningAll := []response.Option{
		{Key: "a", Text: escalationOptionRetry},
		{Key: "b", Text: escalationOptionBackToPlanning},
		{Key: "c", Text: escalationOptionAbandon},
	}

	t.Run("non-planning states offer only Retry and Abandon, recommend Retry", func(t *testing.T) {
		t.Parallel()
		nonPlanningStates := []string{
			ticketStateBuilding, testStateReviewing, testStateJudging, testStateShipping,
		}
		for _, state := range nonPlanningStates {
			for _, code := range []response.EscalationCode{
				response.EscalationCodeResponseInvalid, response.EscalationCodeSplitUnsupported,
				response.EscalationCodeNothingToDoWithTrueClaims, response.EscalationCodeOther,
				response.EscalationCodeReplanUnsupported,
			} {
				opts, recommended := escalationOptionsFor(state, string(code), false)
				if !reflect.DeepEqual(opts, wantPostSeal) {
					t.Errorf("escalationOptionsFor(%q, %q) options = %+v, want %+v", state, code, opts, wantPostSeal)
				}
				if recommended != "a" {
					t.Errorf("escalationOptionsFor(%q, %q) recommended = %q, want %q", state, code, recommended, "a")
				}
			}
		}
	})

	t.Run("planning back-to-planning codes recommend b and offer all three", func(t *testing.T) {
		t.Parallel()
		for _, code := range []response.EscalationCode{
			response.EscalationCodeSplitUnsupported, response.EscalationCodeNothingToDoWithTrueClaims,
		} {
			opts, recommended := escalationOptionsFor(ticketStatePlanning, string(code), false)
			if !reflect.DeepEqual(opts, wantPlanningAll) {
				t.Errorf("escalationOptionsFor(planning, %q) options = %+v, want %+v", code, opts, wantPlanningAll)
			}
			if recommended != "b" {
				t.Errorf("escalationOptionsFor(planning, %q) recommended = %q, want %q", code, recommended, "b")
			}
		}
	})

	t.Run("planning every other code recommends a and offers all three", func(t *testing.T) {
		t.Parallel()
		for _, code := range []response.EscalationCode{
			response.EscalationCodeLoopsExhausted, response.EscalationCodeResponseInvalid,
			response.EscalationCodeRuntimeExecFailed, response.EscalationCodeEnvironment,
			response.EscalationCodeSandboxUnavailable, response.EscalationCodeResumesExhausted,
			response.EscalationCodeWallClock, response.EscalationCodeSealFailed,
			response.EscalationCodePlanGap, response.EscalationCodeCannotRun,
			response.EscalationCodeOther, response.EscalationCodePostRunFailed,
		} {
			opts, recommended := escalationOptionsFor(ticketStatePlanning, string(code), false)
			if !reflect.DeepEqual(opts, wantPlanningAll) {
				t.Errorf("escalationOptionsFor(planning, %q) options = %+v, want %+v", code, opts, wantPlanningAll)
			}
			if recommended != "a" {
				t.Errorf("escalationOptionsFor(planning, %q) recommended = %q, want %q", code, recommended, "a")
			}
		}
	})
}
