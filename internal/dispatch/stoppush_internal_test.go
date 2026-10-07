package dispatch

import (
	"encoding/json"
	"testing"
	"time"
)

// wrappedErrForTest lets a test control an error's Error() string
// independently of the sentinel it unwraps to, so alertCause's cause text
// and alertKindWhere's errors.Is(err, ErrFailClosed) check can be set
// separately (design section "shape" rules' fail-closed ticket 12 example).
type wrappedErrForTest struct {
	msg string
	err error
}

func (w wrappedErrForTest) Error() string { return w.msg }
func (w wrappedErrForTest) Unwrap() error { return w.err }

func TestStopHeadline(t *testing.T) {
	cases := []struct {
		name string
		s    StopStatus
		want string
	}{
		{"not stopped", StopStatus{Stopped: false}, ""},
		{"owner", StopStatus{Stopped: true, Kind: StopKindOwner}, "Dispatching is stopped by the owner."},
		{
			"ticket",
			StopStatus{Stopped: true, Kind: StopKindFailClosed, HasTicket: true, TicketID: 12},
			"Dispatching stopped after fail-closed on ticket 12.",
		},
		{
			"no ticket",
			StopStatus{Stopped: true, Kind: StopKindError},
			"Dispatching stopped after error in a dispatcher pass.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StopHeadline(c.s); got != c.want {
				t.Errorf("StopHeadline(%+v) = %q, want %q", c.s, got, c.want)
			}
		})
	}
}

func TestStopPushPayload(t *testing.T) {
	at := time.Date(2026, 10, 7, 15, 4, 5, 0, time.FixedZone("EDT", -4*3600))

	t.Run("fail-closed ticket 12", func(t *testing.T) {
		err := &runError{
			TicketID: 12,
			Err:      wrappedErrForTest{msg: "the lease was lost", err: ErrFailClosed},
		}
		got := stopPushPayload(err, at)
		want := `{"title":"Zing stopped dispatching","body":"Dispatching stopped after fail-closed on ticket 12. Cause: the lease was lost. Stopped at 15:04:05.","kind":"fail-closed","ticket_id":12,"at":"2026-10-07T15:04:05-04:00"}`
		if string(got) != want {
			t.Errorf("stopPushPayload =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("pass error has no ticket_id", func(t *testing.T) {
		err := wrappedErrForTest{msg: "list ready tickets: boom", err: nil}
		got := stopPushPayload(err, at)
		var sp map[string]any
		if jsonErr := json.Unmarshal(got, &sp); jsonErr != nil {
			t.Fatalf("Unmarshal: %v", jsonErr)
		}
		if _, ok := sp["ticket_id"]; ok {
			t.Errorf("ticket_id present in %s", got)
		}
		wantHeadline := "Dispatching stopped after error in a dispatcher pass."
		body, ok := sp["body"].(string)
		if !ok || body[:len(wantHeadline)] != wantHeadline {
			t.Errorf("body %q does not start with %q", body, wantHeadline)
		}
	})

	t.Run("empty cause has no Cause sentence", func(t *testing.T) {
		err := wrappedErrForTest{msg: "", err: nil}
		got := stopPushPayload(err, at)
		var sp map[string]any
		if jsonErr := json.Unmarshal(got, &sp); jsonErr != nil {
			t.Fatalf("Unmarshal: %v", jsonErr)
		}
		body, ok := sp["body"].(string)
		want := "Dispatching stopped after error in a dispatcher pass. Stopped at 15:04:05."
		if !ok || body != want {
			t.Errorf("body = %q, want %q", body, want)
		}
	})
}
