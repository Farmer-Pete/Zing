package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"zing/internal/console/templates"
	"zing/internal/dispatch"
)

// Dispatch is the one seam between this package and internal/dispatch
// (ticket #89): the console never reaches into a *dispatch.Dispatcher's
// other methods, just these two. *dispatch.Dispatcher satisfies it as-is,
// so cmd/zing/serve.go wires the same value it already runs d.Run on.
type Dispatch interface {
	StopStatus(ctx context.Context) (dispatch.StopStatus, error)
	Resume(ctx context.Context) error
}

// WithDispatch wires the dispatcher behind the #alerts stop banner and
// POST /dispatch/resume (ticket #89). Without it, stopBanner always
// renders nothing and handleDispatchResume always answers 503: every one
// of New's other 15 call sites passes no options and is unaffected.
func WithDispatch(d Dispatch) Option {
	return func(c *console) { c.dispatch = d }
}

// stopBanner reads the wired dispatcher's stop status for #alerts (ticket
// #89). A read error logs at debug only, not warn: a warn line would
// itself publish on the bus (log.go's Handler), triggering another
// render that reads the same failing status again.
func (c *console) stopBanner(ctx context.Context) templates.StopBanner {
	if c.dispatch == nil {
		return templates.StopBanner{}
	}
	status, err := c.dispatch.StopStatus(ctx)
	if err != nil {
		slog.Debug("console: read dispatcher stop status", "err", err)
	}
	return buildStopBanner(status)
}

// buildStopBanner turns the dispatcher's stop status into the #alerts
// banner's view model (ticket #89). A zero StopStatus (Stopped false)
// renders nothing.
func buildStopBanner(s dispatch.StopStatus) templates.StopBanner {
	if !s.Stopped {
		return templates.StopBanner{}
	}
	b := templates.StopBanner{Show: true, Kind: s.Kind, Cause: s.Cause, Headline: dispatch.StopHeadline(s)}
	if s.Kind != dispatch.StopKindOwner && s.HasTicket {
		b.Note = fmt.Sprintf("Ticket %d runs again once its claim expires.", s.TicketID)
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

// handleDispatchResume is POST /dispatch/resume (ticket #89), sat behind
// the same mutation guard every other state-changing route is (mw.go). It
// answers 204 on success, 409 with the dispatcher's own *ResumeRefusal
// sentence verbatim, 500 on any other error, and 503 when no dispatcher is
// wired through WithDispatch.
func (c *console) handleDispatchResume(w http.ResponseWriter, r *http.Request) {
	if c.dispatch == nil {
		http.Error(w, "the dispatcher is not running in this process", http.StatusServiceUnavailable)
		return
	}
	err := c.dispatch.Resume(r.Context())
	if refusal, ok := errors.AsType[*dispatch.ResumeRefusal](err); ok {
		http.Error(w, refusal.Reason, http.StatusConflict)
		return
	}
	if err != nil {
		slog.Error("console: resume dispatcher", "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
