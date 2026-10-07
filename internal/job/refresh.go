// refresh.go reads the issue's current body and the owner's comments from
// the tracker before a planning tick (design "Planning refreshes first"):
// refreshTicket compares what the tracker reports now against what the
// ticket row last stored and, on a change, returns a commit that stores the
// new text and says what changed, instead of running the tick at all.
package job

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"zing/internal/store"
)

// TicketSource reads a ticket's current text from its tracker. The
// dispatcher implements it and passes itself as Deps.Source.
type TicketSource interface {
	IssueText(ctx context.Context, projectID int64, ref string) (IssueText, error)
}

// IssueText is one tracker read of a ticket's issue: the body, the owner's
// comments already filtered and rendered (tracker.OwnerComments,
// tracker.RenderComments), and how many of those comments there were.
type IssueText struct {
	Body          string
	OwnerComments string
	CommentCount  int
}

// ticketRefreshedMarker and ticketRefreshDeliveredMarker are the refresh
// message markers (design "the refresh marker's states"): a live refresh
// (the newer marker, unmatched by a later delivered marker) has not yet
// reached the planner.
const (
	ticketRefreshedMarker        = "ticket refreshed from tracker"
	ticketRefreshDeliveredMarker = "ticket refresh delivered"
)

// refreshDecision compares it against t's last tracker read. baseline is
// t.TrackerBody, or t.Body when the row predates migration 0011 and was
// never edited in the console. It returns the update, the message body,
// and whether anything changed.
func refreshDecision(t store.Ticket, it IssueText) (*store.TicketText, string, bool) {
	baseline := t.Body
	if t.TrackerBody != nil {
		baseline = *t.TrackerBody
	}
	bodyChanged := it.Body != baseline
	commentsChanged := it.OwnerComments != t.OwnerComments
	if !bodyChanged && !commentsChanged {
		return nil, "", false
	}
	upd := &store.TicketText{OwnerComments: it.OwnerComments}
	lines := []string{ticketRefreshedMarker}
	if bodyChanged {
		body := it.Body
		upd.Body = &body
		lines = append(lines, "issue body edited on the tracker")
		switch {
		case t.TrackerBody == nil:
			lines = append(lines, "the new issue body replaced the stored ticket body, which may have held a console edit")
		case t.Body != baseline:
			lines = append(lines, "the new issue body replaced the console edit of the ticket body")
		}
	}
	if commentsChanged {
		lines = append(lines, fmt.Sprintf("owner comments changed, %d now", it.CommentCount))
	}
	return upd, strings.Join(lines, "\n"), true
}

// refreshTicket re-reads t's claimed row (the dispatcher read t before
// claiming, and the console's body edit is refused only once a claim is
// held), reads the tracker through d.Source, and, if the text changed,
// returns a commit carrying only SetTicketText and the refresh message, and
// true. A nil Source, a read or source error (logged at warn), or no change
// returns false, and the caller runs the tick on the stored text.
func refreshTicket(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, bool) {
	if d.Source == nil {
		slog.Debug("ticket refresh: no source", "ticket_id", t.ID)
		return store.HandlerCommit{}, false
	}
	fresh, err := d.Store.GetTicket(ctx, t.ID)
	if err != nil {
		slog.Warn("ticket refresh: re-read failed; using stored text", "ticket_id", t.ID, "err", err)
		return store.HandlerCommit{}, false
	}
	it, err := d.Source.IssueText(ctx, fresh.ProjectID, fresh.TrackerRef)
	if err != nil {
		slog.Warn("ticket refresh failed; using stored text", "ticket_id", t.ID, "ref", fresh.TrackerRef, "err", err)
		return store.HandlerCommit{}, false
	}
	upd, body, changed := refreshDecision(fresh, it)
	if !changed {
		slog.Debug("ticket refresh: unchanged", "ticket_id", t.ID, "ref", fresh.TrackerRef, "owner_comments", it.CommentCount)
		return store.HandlerCommit{}, false
	}
	c := baseCommit(t, d)
	c.SetTicketText = upd
	c.Messages = []store.Message{{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: body}}
	slog.Info("ticket refreshed from tracker", "ticket_id", t.ID, "ref", fresh.TrackerRef, "body_changed", upd.Body != nil, "owner_comments", it.CommentCount)
	return c, true
}

// refreshLive reports whether a refresh message has no delivered message
// after it (design "the refresh marker's states"): the planner has not yet
// seen the text a refresh commit stored.
func refreshLive(ctx context.Context, d Deps, ticketID int64) (bool, error) {
	_, live, err := d.Store.LiveMarker(ctx, ticketID, ticketRefreshedMarker, ticketRefreshDeliveredMarker)
	if err != nil {
		return false, fmt.Errorf("job: refresh marker: %w", err)
	}
	return live, nil
}

// markRefreshDelivered appends the delivered message to commit when live,
// err is nil, and the run reserved a row (commit.Runs non-empty). In every
// other case it returns commit and err unchanged.
func markRefreshDelivered(commit store.HandlerCommit, err error, live bool, ticketID int64) (store.HandlerCommit, error) {
	if live && err == nil && len(commit.Runs) > 0 {
		commit.Messages = append(commit.Messages, store.Message{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: ticketRefreshDeliveredMarker})
	}
	return commit, err
}
