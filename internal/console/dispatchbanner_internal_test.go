package console

import (
	"testing"
	"time"

	"zing/internal/dispatch"
)

// TestBuildStopBanner proves buildStopBanner's three headline shapes and its
// InFlight-driven button state (ticket #89 design "shape" rules worked
// example).
func TestBuildStopBanner(t *testing.T) {
	t.Parallel()

	stoppedAt := time.Date(2024, 1, 2, 14, 3, 9, 0, time.Local)

	cases := []struct {
		name string
		in   dispatch.StopStatus
		want stopBannerWant
	}{
		{
			name: "not stopped",
			in:   dispatch.StopStatus{},
			want: stopBannerWant{},
		},
		{
			name: "owner",
			in:   dispatch.StopStatus{Stopped: true, Kind: dispatch.StopKindOwner},
			want: stopBannerWant{
				show:     true,
				kind:     dispatch.StopKindOwner,
				headline: "Dispatching is stopped by the owner.",
			},
		},
		{
			name: "fail-closed worked example",
			in: dispatch.StopStatus{
				Stopped:   true,
				Kind:      dispatch.StopKindFailClosed,
				Cause:     "dispatch: fail-closed: commit not applied cleanly: ticket 42: the lease was lost",
				TicketID:  42,
				HasTicket: true,
				At:        stoppedAt,
				InFlight:  1,
			},
			want: stopBannerWant{
				show:           true,
				kind:           dispatch.StopKindFailClosed,
				headline:       "Dispatching stopped after fail-closed on ticket 42.",
				cause:          "dispatch: fail-closed: commit not applied cleanly: ticket 42: the lease was lost",
				note:           "Ticket 42 runs again once its claim expires.",
				time:           stoppedAt.Format(alertLineTimeFormat),
				resumeDisabled: true,
				busy:           "1 runs are still finishing; resume once they are done",
			},
		},
		{
			name: "error with no ticket",
			in:   dispatch.StopStatus{Stopped: true, Kind: dispatch.StopKindError},
			want: stopBannerWant{
				show:     true,
				kind:     dispatch.StopKindError,
				headline: "Dispatching stopped after error in a dispatcher pass.",
			},
		},
		{
			name: "InFlight 0 leaves the button enabled",
			in:   dispatch.StopStatus{Stopped: true, Kind: dispatch.StopKindError, InFlight: 0},
			want: stopBannerWant{
				show:     true,
				kind:     dispatch.StopKindError,
				headline: "Dispatching stopped after error in a dispatcher pass.",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildStopBanner(tc.in)
			if got.Show != tc.want.show {
				t.Errorf("Show = %v, want %v", got.Show, tc.want.show)
			}
			if got.Kind != tc.want.kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.want.kind)
			}
			if got.Headline != tc.want.headline {
				t.Errorf("Headline = %q, want %q", got.Headline, tc.want.headline)
			}
			if got.Cause != tc.want.cause {
				t.Errorf("Cause = %q, want %q", got.Cause, tc.want.cause)
			}
			if got.Note != tc.want.note {
				t.Errorf("Note = %q, want %q", got.Note, tc.want.note)
			}
			if got.Time != tc.want.time {
				t.Errorf("Time = %q, want %q", got.Time, tc.want.time)
			}
			if got.ResumeDisabled != tc.want.resumeDisabled {
				t.Errorf("ResumeDisabled = %v, want %v", got.ResumeDisabled, tc.want.resumeDisabled)
			}
			if got.Busy != tc.want.busy {
				t.Errorf("Busy = %q, want %q", got.Busy, tc.want.busy)
			}
		})
	}
}

// stopBannerWant is this test's own expectation shape, named
// distinctly from templates.StopBanner so a future field added to one does
// not silently compile against the other's zero value.
type stopBannerWant struct {
	show           bool
	kind           string
	headline       string
	cause          string
	note           string
	time           string
	resumeDisabled bool
	busy           string
}
