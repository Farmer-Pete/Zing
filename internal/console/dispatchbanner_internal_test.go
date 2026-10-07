package console

import (
	"testing"
	"time"

	"zing/internal/console/templates"
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
		want templates.StopBanner
	}{
		{
			name: "not stopped",
			in:   dispatch.StopStatus{},
			want: templates.StopBanner{},
		},
		{
			name: "owner",
			in:   dispatch.StopStatus{Stopped: true, Kind: dispatch.StopKindOwner},
			want: templates.StopBanner{
				Show:     true,
				Kind:     dispatch.StopKindOwner,
				Headline: "Dispatching is stopped by the owner.",
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
			want: templates.StopBanner{
				Show:           true,
				Kind:           dispatch.StopKindFailClosed,
				Headline:       "Dispatching stopped after fail-closed on ticket 42.",
				Cause:          "dispatch: fail-closed: commit not applied cleanly: ticket 42: the lease was lost",
				Note:           "Ticket 42 runs again once its claim expires.",
				Time:           stoppedAt.Format(alertLineTimeFormat),
				ResumeDisabled: true,
				Busy:           "1 runs are still finishing; resume once they are done",
			},
		},
		{
			name: "error with no ticket",
			in:   dispatch.StopStatus{Stopped: true, Kind: dispatch.StopKindError},
			want: templates.StopBanner{
				Show:     true,
				Kind:     dispatch.StopKindError,
				Headline: "Dispatching stopped after error in a dispatcher pass.",
			},
		},
		{
			name: "InFlight 0 leaves the button enabled",
			in:   dispatch.StopStatus{Stopped: true, Kind: dispatch.StopKindError, InFlight: 0},
			want: templates.StopBanner{
				Show:     true,
				Kind:     dispatch.StopKindError,
				Headline: "Dispatching stopped after error in a dispatcher pass.",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildStopBanner(tc.in)
			if got != tc.want {
				t.Errorf("buildStopBanner(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}
