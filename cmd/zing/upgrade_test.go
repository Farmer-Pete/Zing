package main

import (
	"path/filepath"
	"strings"
	"testing"

	"zing/internal/store"
)

// TestRedactURLs proves redactURLs replaces a URL's userinfo with REDACTED
// and leaves text with no URL unchanged.
func TestRedactURLs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "https with user and token",
			in:   "https://user:token@github.com/x/zing.git",
			want: "https://REDACTED@github.com/x/zing.git",
		},
		{
			name: "ssh with git user",
			in:   "ssh://git@github.com/x/zing.git",
			want: "ssh://REDACTED@github.com/x/zing.git",
		},
		{
			name: "two URLs on one line",
			in:   "from https://a:b@h1/x to ssh://c@h2/y",
			want: "from https://REDACTED@h1/x to ssh://REDACTED@h2/y",
		},
		{
			name: "no URL",
			in:   "upgrade: build failed: exit status 1",
			want: "upgrade: build failed: exit status 1",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := redactURLs(c.in); got != c.want {
				t.Errorf("redactURLs(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// seedTicketForUpgrade inserts one ticket directly into st, the way
// serve_test.go's seedQueuedTicketForServe seeds a ticket through the
// store, so tellOwner has somewhere to post.
func seedTicketForUpgrade(t *testing.T, st *store.Store) int64 {
	t.Helper()

	projectID, err := st.EnsureProject(t.Context(), store.Project{
		Name: "zing", RepoURL: "https://github.com/x/zing", Tracker: "github",
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := st.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "1", Title: "a ticket", State: "queued",
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return ticketID
}

// TestTellOwner_PostsOnlyForTicket proves tellOwner posts one redacted
// update message by author system when ticketID is above 0, and posts no
// message at all for ticket id 0.
func TestTellOwner_PostsOnlyForTicket(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	ticketID := seedTicketForUpgrade(t, st)

	tellOwner(t.Context(), st, ticketID, "upgrade: build failed: https://u:tok@h/x")

	msgs, err := st.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("ListMessages = %d messages, want 1", len(msgs))
	}
	if msgs[0].Type != "update" || msgs[0].Author != "system" {
		t.Errorf("message type/author = %s/%s, want update/system", msgs[0].Type, msgs[0].Author)
	}
	if strings.Contains(msgs[0].Body, "tok") {
		t.Errorf("message body %q still contains the secret", msgs[0].Body)
	}
	if !strings.Contains(msgs[0].Body, "REDACTED") {
		t.Errorf("message body %q does not contain REDACTED", msgs[0].Body)
	}

	tellOwner(t.Context(), st, 0, "upgrade: no ticket for this one")

	after, err := st.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("ListMessages after ticket_id 0 = %d messages, want still 1", len(after))
	}
}
