// answer_internal_test.go is a whitebox test for sendResultText (answer.go):
// unexported, so it lives in package console rather than console_test, the
// same views_internal_test.go and rail_internal_test.go precedent.
package console

import (
	"testing"

	"zing/internal/store"
)

// TestSendResultTextCountsMessagesAndDiscards proves sendResultText's own
// text (design section 22.7): "Sent 1 message." singular, "Sent N
// messages." plural, off BatchResult.Sent, plus, only when Discarded > 0,
// " 1 not sent: its question closed first." or " N not sent: ...", so a
// batch that both sent and discarded reports both halves.
func TestSendResultTextCountsMessagesAndDiscards(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		result store.BatchResult
		want   string
	}{
		{"one sent, singular", store.BatchResult{Sent: 1}, "Sent 1 message."},
		{"several sent, plural", store.BatchResult{Sent: 3}, "Sent 3 messages."},
		{
			"one sent, one discarded",
			store.BatchResult{Sent: 1, Discarded: 1},
			"Sent 1 message. 1 not sent: its question closed first.",
		},
		{
			"several sent, several discarded",
			store.BatchResult{Sent: 2, Discarded: 3},
			"Sent 2 messages. 3 not sent: its question closed first.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sendResultText(tc.result); got != tc.want {
				t.Errorf("sendResultText(%+v) = %q, want %q", tc.result, got, tc.want)
			}
		})
	}
}
