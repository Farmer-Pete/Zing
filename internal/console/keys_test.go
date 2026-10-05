package console_test

import (
	"bytes"
	"flag"
	"os"
	"testing"
	"unicode/utf8"

	"zing/internal/console"
)

var updateKeys = flag.Bool("update", false, "update the committed internal/console/static/keys.json")

const keysJSONPath = "static/keys.json"

// TestKeysJSONMatchesCommitted is the golden test keys.go's go:generate
// directive drives: run with -update to (re)write static/keys.json from
// console.GenerateKeysJSON(); without it, the committed file's bytes must
// equal that output exactly, so a hand-edit or a stale commit fails here
// the same way internal/schemagen.TestGenerate_MatchesCommitted catches
// schema drift (design section 6.4, 7.3: "asserts keys.json is current
// against keys.go").
func TestKeysJSONMatchesCommitted(t *testing.T) {
	t.Parallel()
	generated, err := console.GenerateKeysJSON()
	if err != nil {
		t.Fatalf("GenerateKeysJSON: %v", err)
	}

	if *updateKeys {
		if writeErr := os.WriteFile(keysJSONPath, generated, 0o644); writeErr != nil {
			t.Fatalf("write %s: %v", keysJSONPath, writeErr)
		}
	}

	got, err := os.ReadFile(keysJSONPath)
	if err != nil {
		t.Fatalf("read committed %s: %v", keysJSONPath, err)
	}
	if !bytes.Equal(got, generated) {
		t.Errorf("%s differs from console.GenerateKeysJSON() output; run `go generate ./internal/console`", keysJSONPath)
	}
}

// every14Key is design section 8's closed key set, spelled out one literal
// key string per element (§14's "1..9" and "Cmd-Enter/Ctrl-Enter" expanded
// to their individual keys, and "Enter" and "Enter-in-input" kept as the
// two distinct bindings section 6.4 requires). keys_test.go treats this
// list, not the prose, as the authoritative closed set to check Bindings
// against.
var every14Key = []string{
	"g i", "g r", "g f",
	"j", "k",
	"o", "Enter",
	"u",
	"Tab", "Shift-Tab",
	"Enter-in-input",
	"1", "2", "3", "4", "5", "6", "7", "8", "9",
	"Cmd-Enter", "Ctrl-Enter",
	"a", "b", "?", "Esc",
}

// TestBindingsCoverEvery14KeyExactlyOnce proves every §14 key is bound to
// exactly one action: every key in every14Key appears in exactly one
// KeyBinding.Keys, and Bindings names no key outside that closed set
// (design section 8: "Each maps to exactly one action").
func TestBindingsCoverEvery14KeyExactlyOnce(t *testing.T) {
	t.Parallel()
	counts := make(map[string]int)
	for _, b := range console.Bindings() {
		for _, k := range b.Keys {
			counts[k]++
		}
	}

	want := make(map[string]bool, len(every14Key))
	for _, k := range every14Key {
		want[k] = true
		if counts[k] != 1 {
			t.Errorf("key %q appears %d times across Bindings(), want exactly 1", k, counts[k])
		}
	}
	for k := range counts {
		if !want[k] {
			t.Errorf("Bindings() names key %q, which is not in design section 8's closed key set", k)
		}
	}
	if len(every14Key) != len(want) {
		t.Fatalf("every14Key has a duplicate entry: %d listed, %d unique", len(every14Key), len(want))
	}
}

// bareKeyForbidden names every action a single bare keystroke must never
// trigger, because the action writes to the server: stop, stop-all, and
// mark-read post to the ticket/global/message store directly on keydown, and
// draft and send post the composer's batch. chip is deliberately absent: it
// also writes (POST /draft), but the owner decided on 2026-10-04 that the
// digit keys stay bare because a draft is reversible.
var bareKeyForbidden = map[string]bool{
	"stop":      true,
	"stop-all":  true,
	"mark-read": true,
	"draft":     true,
	"send":      true,
}

// TestBindingsNoBareKeyWritesState proves no single printable-character key
// is bound to a server-writing action (ticket #44, bugs 2 and 3 of #106): a
// stray letter typed outside a text box must never change state. A key of
// exactly one rune is "bare" in this sense; chords like "g i" or modified
// keys like "Cmd-Enter" are not.
func TestBindingsNoBareKeyWritesState(t *testing.T) {
	t.Parallel()
	for _, b := range console.Bindings() {
		if !bareKeyForbidden[b.Action] {
			continue
		}
		for _, k := range b.Keys {
			if utf8.RuneCountInString(k) == 1 {
				t.Errorf("key %q is bound to %q, a server-writing action; no single bare key may trigger it", k, b.Action)
			}
		}
	}
}

// TestBindingsActionsAreNonEmptyAndKeysNonEmpty proves every row is usable:
// no binding with a blank action, and no binding with zero keys (which
// would silently drop an action out of the closed-set coverage above
// without failing it).
func TestBindingsActionsAreNonEmptyAndKeysNonEmpty(t *testing.T) {
	t.Parallel()
	for _, b := range console.Bindings() {
		if b.Action == "" {
			t.Errorf("KeyBinding with Keys=%v has an empty Action", b.Keys)
		}
		if len(b.Keys) == 0 {
			t.Errorf("KeyBinding with Action=%q has no Keys", b.Action)
		}
	}
}
