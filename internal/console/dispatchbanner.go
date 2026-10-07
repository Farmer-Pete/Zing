package console

import (
	"fmt"

	"zing/internal/console/templates"
	"zing/internal/dispatch"
)

// buildStopBanner turns the dispatcher's stop status into the #alerts
// banner's view model (ticket #89). A zero StopStatus (Stopped false)
// renders nothing.
func buildStopBanner(s dispatch.StopStatus) templates.StopBanner {
	if !s.Stopped {
		return templates.StopBanner{}
	}
	b := templates.StopBanner{Show: true, Kind: s.Kind, Cause: s.Cause}
	switch {
	case s.Kind == dispatch.StopKindOwner:
		b.Headline = "Dispatching is stopped by the owner."
	case s.HasTicket:
		b.Headline = fmt.Sprintf("Dispatching stopped after %s on ticket %d.", s.Kind, s.TicketID)
		b.Note = fmt.Sprintf("Ticket %d runs again once its claim expires.", s.TicketID)
	default:
		b.Headline = fmt.Sprintf("Dispatching stopped after %s in a dispatcher pass.", s.Kind)
	}
	if !s.At.IsZero() {
		b.Time = s.At.Format(alertLineTimeFormat)
	}
	if s.InFlight > 0 {
		b.ResumeDisabled = true
		b.Busy = fmt.Sprintf("%d runs are still finishing; resume once they are done", s.InFlight)
	}
	return b
}
