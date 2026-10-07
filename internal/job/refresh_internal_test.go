package job

// refresh_internal_test.go tests refresh.go's own pure decision function,
// refreshDecision (#98, design "refreshDecision"), with store.Ticket and
// IssueText built in memory. refresh_test.go (package job_test) drives
// refreshTicket and planningHandler.Run through the real store.

import (
	"errors"
	"testing"

	"zing/internal/store"
)

// refreshTestTrackerBody is the one TrackerBody (the last tracker read)
// every refreshTestTicket case below shares; only Body (possibly a console
// edit on top of it) and OwnerComments vary.
const refreshTestTrackerBody = "A"

// refreshTestTicket builds a store.Ticket whose TrackerBody is
// refreshTestTrackerBody and whose Body is body, with ownerComments as the
// ticket's own stored comments.
func refreshTestTicket(body, ownerComments string) store.Ticket {
	tb := refreshTestTrackerBody
	return store.Ticket{Body: body, TrackerBody: &tb, OwnerComments: ownerComments}
}

// TestRefreshDecision proves refreshDecision's table of cases (design
// "refreshDecision", worked example): no change at all, a comments-only
// change, a body change with no console edit, a body change over a console
// edit, a nil TrackerBody (pre-migration row) with and without a body
// change, and comments cleared to "".
func TestRefreshDecision(t *testing.T) {
	t.Parallel()

	t.Run("unchanged", func(t *testing.T) {
		t.Parallel()
		ticket := refreshTestTicket("A", "")
		upd, msg, changed := refreshDecision(ticket, IssueText{Body: "A", OwnerComments: ""})
		if changed || upd != nil || msg != "" {
			t.Errorf("refreshDecision = (%v, %q, %v), want (nil, \"\", false)", upd, msg, changed)
		}
	})

	t.Run("comments_only", func(t *testing.T) {
		t.Parallel()
		ticket := refreshTestTicket("A", "")
		upd, msg, changed := refreshDecision(ticket, IssueText{Body: "A", OwnerComments: "Comment by owner:\nUse serve.", CommentCount: 1})
		if !changed {
			t.Fatal("refreshDecision changed = false, want true")
		}
		if upd == nil || upd.Body != nil || upd.OwnerComments != "Comment by owner:\nUse serve." {
			t.Errorf("refreshDecision upd = %+v, want Body nil, OwnerComments set", upd)
		}
		want := ticketRefreshedMarker + "\nowner comments changed, 1 now"
		if msg != want {
			t.Errorf("refreshDecision msg = %q, want %q", msg, want)
		}
	})

	t.Run("body_changed_no_console_edit", func(t *testing.T) {
		t.Parallel()
		ticket := refreshTestTicket("A", "")
		upd, msg, changed := refreshDecision(ticket, IssueText{Body: "B"})
		if !changed {
			t.Fatal("refreshDecision changed = false, want true")
		}
		if upd == nil || upd.Body == nil || *upd.Body != "B" {
			t.Errorf("refreshDecision upd = %+v, want Body \"B\"", upd)
		}
		want := ticketRefreshedMarker + "\nissue body edited on the tracker"
		if msg != want {
			t.Errorf("refreshDecision msg = %q, want %q", msg, want)
		}
	})

	t.Run("body_changed_over_console_edit", func(t *testing.T) {
		t.Parallel()
		ticket := refreshTestTicket("A, edited", "")
		upd, msg, changed := refreshDecision(ticket, IssueText{Body: "B"})
		if !changed {
			t.Fatal("refreshDecision changed = false, want true")
		}
		if upd == nil || upd.Body == nil || *upd.Body != "B" {
			t.Errorf("refreshDecision upd = %+v, want Body \"B\"", upd)
		}
		want := ticketRefreshedMarker + "\nissue body edited on the tracker" +
			"\nthe new issue body replaced the console edit of the ticket body"
		if msg != want {
			t.Errorf("refreshDecision msg = %q, want %q", msg, want)
		}
	})

	t.Run("nil_tracker_body_changed", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Body: "A, edited"}
		upd, msg, changed := refreshDecision(ticket, IssueText{Body: "B"})
		if !changed {
			t.Fatal("refreshDecision changed = false, want true")
		}
		if upd == nil || upd.Body == nil || *upd.Body != "B" {
			t.Errorf("refreshDecision upd = %+v, want Body \"B\"", upd)
		}
		want := ticketRefreshedMarker + "\nissue body edited on the tracker" +
			"\nthe new issue body replaced the stored ticket body, which may have held a console edit"
		if msg != want {
			t.Errorf("refreshDecision msg = %q, want %q", msg, want)
		}
	})

	t.Run("nil_tracker_body_unchanged", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Body: "A"}
		upd, msg, changed := refreshDecision(ticket, IssueText{Body: "A"})
		if changed || upd != nil || msg != "" {
			t.Errorf("refreshDecision = (%v, %q, %v), want (nil, \"\", false)", upd, msg, changed)
		}
	})

	t.Run("comments_cleared", func(t *testing.T) {
		t.Parallel()
		ticket := refreshTestTicket("A", "Comment by owner:\nUse serve.")
		upd, msg, changed := refreshDecision(ticket, IssueText{Body: "A", OwnerComments: "", CommentCount: 0})
		if !changed {
			t.Fatal("refreshDecision changed = false, want true")
		}
		if upd == nil || upd.Body != nil || upd.OwnerComments != "" {
			t.Errorf("refreshDecision upd = %+v, want Body nil, OwnerComments \"\"", upd)
		}
		want := ticketRefreshedMarker + "\nowner comments changed, 0 now"
		if msg != want {
			t.Errorf("refreshDecision msg = %q, want %q", msg, want)
		}
	})
}

// TestMarkRefreshDelivered proves markRefreshDelivered's table of cases
// (#98, design "markRefreshDelivered"): only a live marker, a nil err, and
// a reserved run (commit.Runs non-empty) earns the delivered message.
func TestMarkRefreshDelivered(t *testing.T) {
	t.Parallel()

	runs := []store.Run{{ID: 1}}

	t.Run("live_with_run_appends_delivered", func(t *testing.T) {
		t.Parallel()
		commit := store.HandlerCommit{TicketID: 7, Runs: runs}
		got, err := markRefreshDelivered(commit, nil, true, 7)
		if err != nil {
			t.Fatalf("markRefreshDelivered err = %v, want nil", err)
		}
		if len(got.Messages) != 1 || got.Messages[0].Body != ticketRefreshDeliveredMarker || got.Messages[0].TicketID != 7 {
			t.Errorf("got.Messages = %+v, want one %q message for ticket 7", got.Messages, ticketRefreshDeliveredMarker)
		}
	})

	t.Run("not_live_appends_nothing", func(t *testing.T) {
		t.Parallel()
		commit := store.HandlerCommit{TicketID: 7, Runs: runs}
		got, err := markRefreshDelivered(commit, nil, false, 7)
		if err != nil {
			t.Fatalf("markRefreshDelivered err = %v, want nil", err)
		}
		if len(got.Messages) != 0 {
			t.Errorf("got.Messages = %+v, want none", got.Messages)
		}
	})

	t.Run("live_with_error_appends_nothing_and_keeps_error", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("boom")
		commit := store.HandlerCommit{TicketID: 7, Runs: runs}
		got, err := markRefreshDelivered(commit, wantErr, true, 7)
		if !errors.Is(err, wantErr) {
			t.Errorf("markRefreshDelivered err = %v, want %v", err, wantErr)
		}
		if len(got.Messages) != 0 {
			t.Errorf("got.Messages = %+v, want none", got.Messages)
		}
	})

	t.Run("live_with_no_runs_appends_nothing", func(t *testing.T) {
		t.Parallel()
		commit := store.HandlerCommit{TicketID: 7}
		got, err := markRefreshDelivered(commit, nil, true, 7)
		if err != nil {
			t.Fatalf("markRefreshDelivered err = %v, want nil", err)
		}
		if len(got.Messages) != 0 {
			t.Errorf("got.Messages = %+v, want none", got.Messages)
		}
	})
}
