// Package templates holds the console's templ components (design section
// 6.2: every console template is a .templ component, and templ generate
// turns each into a committed *_templ.go). It is a package of its own,
// rather than living inside internal/console, only because a .templ file's
// generated *_templ.go is Go source and Go compiles one package per
// directory; internal/console (server.go, handlers.go, render.go) builds
// the view models this package renders and imports it to do so.
//
// MessageOption, QuestionView, and MessageView are the small view-model
// types the thread fragment renders: the presentation shape internal/console
// builds from store rows (render.go's buildMessageViews), not store rows
// themselves, so this package stays free of any question-lifecycle or
// payload-decoding logic of its own.
package templates

// MessageOption is one chip a question block renders: the option key
// POST /answer's $answer signal carries and the button's label text.
type MessageOption struct {
	Key, Text string
}

// QuestionView is the extra data an open question message renders instead
// of its plain Body: the heading and body text split from the stored Body,
// and the chips built from the stored QuestionPayload's options.
type QuestionView struct {
	Title, Body string
	Options     []MessageOption
}

// MessageView is what the thread fragment renders for one message row: the
// type and author as stored, and a display Body that is either the row's
// own Body or a decoded system line. Question is non-nil only for a message
// whose lifecycle state is open, and ThreadFragment renders its question
// block in place of Body for that row.
type MessageView struct {
	ID       int64
	Type     string
	Author   string
	Body     string
	Question *QuestionView
}
