// Package templates holds the console's templ components (design section
// 6.2: every console template is a .templ component, and templ generate
// turns each into a committed *_templ.go). It is a package of its own,
// rather than living inside internal/console, only because a .templ file's
// generated *_templ.go is Go source and Go compiles one package per
// directory; internal/console (server.go, handlers.go, views.go, stream.go)
// builds the view models this package renders and imports it to do so.
//
// This file holds the small view-model types the five views' components
// take as parameters: the presentation shape internal/console/views.go
// builds from store rows, not store rows themselves, so this package stays
// free of any question-lifecycle or payload-decoding logic of its own.
package templates

import "zing/internal/store"

// InboxGroup is one project's cluster of inbox cards, the Inbox view's
// grouping unit (design section 6.5: "grouped by project, blocking first").
// Groups appear in the order their first item was encountered in
// store.InboxItems' own blocking-first, newest-first order, so that overall
// priority order survives the grouping.
type InboxGroup struct {
	ProjectName string
	Items       []store.InboxItem
}

// NavThread is one blocking-or-unread ticket's row in #nav's per-thread
// badge list (design section 6.3, 6.8): the badge shows the waiting flag
// when blocking, an unread dot otherwise.
type NavThread struct {
	Ticket            store.Ticket
	Blocking          bool
	WaitingOn         string // meaningful only when Blocking
	OpenQuestionCount int
}

// ThreadOption is one option a read-only question group lists: a label, not
// a control. Task 3's Thread view carries no chips or composer; Tasks 6 and
// 7 add those over the same option data.
type ThreadOption struct {
	Key, Text string
}

// ThreadQuestion is the read-only detail a "question" message renders in
// place of a plain body: its title and body (split from the stored Body,
// design section 6.6), its recommendation, its options, and a pill label
// for its lifecycle state.
type ThreadQuestion struct {
	Title, Body, Recommended string
	Options                  []ThreadOption
	StateLabel               string
}

// ThreadRow is one message the read-only Thread view renders: a state
// separator (Type == "state"), or a plain row (update, escalation, or any
// other type) with Body as its already-decoded display text, or, when
// Question is non-nil, a read-only question group in place of Body (design
// section 6.6).
type ThreadRow struct {
	ID       int64
	Type     string
	Author   string
	Body     string
	Question *ThreadQuestion
}

// IsState reports whether this row is a state-transition separator, the one
// row type the Thread view centers rather than left-aligning (design
// section 6.6).
func (r ThreadRow) IsState() bool { return r.Type == "state" }
