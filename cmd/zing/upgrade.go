// upgrade.go implements the self-upgrade upgrader: building a merged self
// pull request, testing it, backing up the database, and handing off a
// restart target after serve drains.
package main

import (
	"context"
	"log/slog"
	"regexp"

	"zing/internal/store"
)

// urlUserinfo matches the userinfo component of a URL, so redactURLs can
// strip it before a message reaches a log or a ticket.
var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)

// redactURLs turns scheme://USERINFO@ into scheme://REDACTED@.
func redactURLs(s string) string { return urlUserinfo.ReplaceAllString(s, "${1}REDACTED@") }

// tellOwner logs text at WARN, which the alerts view shows, and posts it on
// ticketID as a system update message when ticketID is above 0.
func tellOwner(ctx context.Context, st *store.Store, ticketID int64, text string) {
	text = redactURLs(text)
	slog.Warn("upgrade", "ticket_id", ticketID, "error", text)
	if ticketID <= 0 {
		return
	}
	if _, err := st.InsertMessage(ctx, store.Message{TicketID: ticketID, Type: "update", Author: "system", Body: text}); err != nil {
		slog.Warn("upgrade: post message", "ticket_id", ticketID, "error", err)
	}
}
