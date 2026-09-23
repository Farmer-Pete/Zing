// keys.go: the console's keyboard map as data (design section 6.4, 7.3,
// closed set in section 8). One KeyBinding row per action, naming every
// literal key or chord string that triggers it. This is the single source:
// the directive below emits static/keys.json from Bindings, and
// keys_test.go asserts that file is current. console.js, the help
// overlay, and the header hints all read the generated JSON; none of them
// import this file.
//
//go:generate go test . -run TestKeysJSONMatchesCommitted -update
package console

import "encoding/json"

// KeyBinding is one row of the keyboard map: the literal key or chord
// strings that trigger one action. Several keys can share an action (o and
// Enter both open; Cmd-Enter and Ctrl-Enter both send), but design section
// 8's closed set gives each individual key exactly one action, so no key
// string appears in more than one KeyBinding's Keys (keys_test.go asserts
// this).
type KeyBinding struct {
	Keys   []string `json:"keys"`
	Action string   `json:"action"`
}

// Bindings is the full §14 keyboard map, exactly design section 8's closed
// key set, the single source static/keys.json is generated from.
//
// "Enter" and "Enter-in-input" are two distinct keys in the closed set, not
// one: plain Enter (like o) opens a focused list row, while Enter inside a
// question input queues a draft (section 6.4, "Inputs"). They share one
// physical key, so console.js tells them apart by input-context before it
// resolves either to an action; keys.go still lists them as two rows
// because they are two different bindings with two different actions.
func Bindings() []KeyBinding {
	return []KeyBinding{
		{Keys: []string{"g i"}, Action: "nav-inbox"},
		{Keys: []string{"g r"}, Action: "nav-recent"},
		{Keys: []string{"g f"}, Action: "nav-feed"},
		{Keys: []string{"j"}, Action: "focus-next"},
		{Keys: []string{"k"}, Action: "focus-prev"},
		{Keys: []string{"o", "Enter"}, Action: "open"},
		{Keys: []string{"u"}, Action: "up"},
		{Keys: []string{"Tab"}, Action: "input-next"},
		{Keys: []string{"Shift-Tab"}, Action: "input-prev"},
		{Keys: []string{"Enter-in-input"}, Action: "draft"},
		{Keys: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}, Action: "chip"},
		{Keys: []string{"Cmd-Enter", "Ctrl-Enter"}, Action: "send"},
		{Keys: []string{"a"}, Action: "toggle-rail"},
		{Keys: []string{"b"}, Action: "focus-side"},
		{Keys: []string{"s"}, Action: "stop"},
		{Keys: []string{"S"}, Action: "stop-all"},
		{Keys: []string{"x"}, Action: "mark-read"},
		{Keys: []string{"?"}, Action: "help"},
		{Keys: []string{"Esc"}, Action: "blur"},
	}
}

// GenerateKeysJSON marshals Bindings into the exact bytes static/keys.json
// commits: two-space-indented JSON with one trailing newline, matching
// internal/schemagen.Generate's convention so both generated-and-committed
// assets in this repo read the same way.
func GenerateKeysJSON() ([]byte, error) {
	b, err := json.MarshalIndent(Bindings(), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
