package templates

import (
	"testing"

	"zing/internal/response"
)

// TestDisplayDecision proves DisplayDecision's own mapping (bug fix, #78):
// a stored reject (perimeter's own word for "do not apply") shows as drop,
// every other decision, including the empty "no pick" value, shows as
// itself.
func TestDisplayDecision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		d    response.Decision
		want response.Decision
	}{
		{"reject shows as drop", response.DecisionReject, response.DecisionDrop},
		{"accept shows as accept", response.DecisionAccept, response.DecisionAccept},
		{"drop shows as drop", response.DecisionDrop, response.DecisionDrop},
		{"discuss shows as discuss", response.DecisionDiscuss, response.DecisionDiscuss},
		{"no pick shows as no pick", response.Decision(""), response.Decision("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DisplayDecision(tc.d); got != tc.want {
				t.Errorf("DisplayDecision(%q) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}
