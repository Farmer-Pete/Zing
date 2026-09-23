package console_test

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/console"
)

// splitQuestionGroups splits one rendered #main thread frame into its
// per-question <details class="q" ...> fragments, in document order, so
// each kind's assertions run against only its own group rather than the
// whole frame (where, e.g., every option kind's chips look alike). The
// first split part -- everything before the first group -- is dropped.
func splitQuestionGroups(t *testing.T, main string) []string {
	t.Helper()
	parts := strings.Split(main, `<details class="q"`)
	if len(parts) < 2 {
		t.Fatalf("splitQuestionGroups: no <details class=\"q\"> groups found in:\n%s", main)
	}
	return parts[1:]
}

// findGroup returns the one group in groups whose title text appears in it,
// failing the test if none or more than one does, so each kind's assertions
// run against exactly the group SeedQuestionFixtures wrote for that kind.
func findGroup(t *testing.T, groups []string, title string) string {
	t.Helper()
	var found string
	matches := 0
	for _, g := range groups {
		if strings.Contains(g, title) {
			found = g
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("findGroup(%q): matched %d groups, want exactly 1", title, matches)
	}
	return found
}

// TestQuestionKindsRenderTheirControls proves the Task 6 dispatch table
// (design section 6.6): the four option kinds (question, gate, split,
// merge) render numbered option chips, the two item kinds (perimeter,
// review) render one row per item with all four decision controls and the
// item's ref, and every kind renders a free reply input wired to
// console.js's postDraft contract (data-draft-ticket, data-draft-question).
// It seeds all six kinds through SeedQuestionFixtures -- the real store, the
// real validated inserts, no hand-built payload -- then reads them back
// over the live /stream the browser itself uses.
func TestQuestionKindsRenderTheirControls(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	if err := console.SeedQuestionFixtures(t.Context(), s, ticketID); err != nil {
		t.Fatalf("SeedQuestionFixtures: %v", err)
	}

	srv := httptest.NewServer(console.New(s, bus.New(), nil, testBindHost, testConsolePort, newTestLogHandler(t)))
	defer srv.Close()

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	_, main, _ := readInitialFrames(t, r)
	assertExactSSEFraming(t, main)

	groups := splitQuestionGroups(t, main)
	if len(groups) != 6 {
		t.Fatalf("got %d question groups, want 6 (one per closed-set kind)", len(groups))
	}

	ticketAttr := `data-draft-ticket="` + strconv.FormatInt(ticketID, 10) + `"`

	t.Run("question kind renders two numbered chips and a free reply", func(t *testing.T) {
		g := findGroup(t, groups, "How should the greeting read?")
		assertChip(t, g, 1, "a")
		assertChip(t, g, 2, "b")
		assertFreeReply(t, g, ticketAttr)
		assertNoItemControls(t, g)
	})

	t.Run("gate kind renders its placeholder context plus chips", func(t *testing.T) {
		g := findGroup(t, groups, "Approve the plan?")
		if !strings.Contains(g, `class="q-context gate-context"`) {
			t.Errorf("gate group missing its gate-context placeholder; got:\n%s", g)
		}
		assertChip(t, g, 1, "a")
		assertChip(t, g, 2, "b")
		assertFreeReply(t, g, ticketAttr)
	})

	t.Run("split kind renders its placeholder context plus chips", func(t *testing.T) {
		g := findGroup(t, groups, "Split this ticket?")
		if !strings.Contains(g, `class="q-context split-context"`) {
			t.Errorf("split group missing its split-context placeholder; got:\n%s", g)
		}
		assertChip(t, g, 1, "a")
		assertChip(t, g, 2, "b")
		assertFreeReply(t, g, ticketAttr)
	})

	t.Run("merge kind renders its PR-link context plus chips", func(t *testing.T) {
		g := findGroup(t, groups, "Merge the PR?")
		if !strings.Contains(g, `class="q-context merge-context"`) {
			t.Errorf("merge group missing its merge-context region; got:\n%s", g)
		}
		if !strings.Contains(g, "No PR yet.") {
			t.Errorf("merge group with no ticket.PRURL set should show the empty state; got:\n%s", g)
		}
		assertChip(t, g, 1, "a")
		assertChip(t, g, 2, "b")
		assertFreeReply(t, g, ticketAttr)
	})

	t.Run("perimeter kind renders one row per item with all four decisions", func(t *testing.T) {
		g := findGroup(t, groups, "Confirm the file perimeter")
		assertItemRow(t, g, "internal/hello/handler.go")
		assertItemRow(t, g, "internal/hello/handler_test.go")
		assertItemRow(t, g, "cmd/zing/main.go")
		assertFreeReply(t, g, ticketAttr)
		if strings.Contains(g, `class="chips"`) {
			t.Errorf("perimeter (an item kind) must not render option chips; got:\n%s", g)
		}
	})

	t.Run("review kind renders one row per item with all four decisions", func(t *testing.T) {
		g := findGroup(t, groups, "Triage the review findings")
		assertItemRow(t, g, "F1")
		assertItemRow(t, g, "F2")
		assertFreeReply(t, g, ticketAttr)
		if strings.Contains(g, `class="chips"`) {
			t.Errorf("review (an item kind) must not render option chips; got:\n%s", g)
		}
	})
}

// assertChip fails the test unless group contains a chip numbered n (the
// keyboard's 1-based data-chip-index) carrying data-option=key, proving the
// option kinds' chips are numbered for console.js's pickChip.
func assertChip(t *testing.T, group string, n int, key string) {
	t.Helper()
	want := `data-chip-index="` + strconv.Itoa(n) + `"`
	if !strings.Contains(group, want) {
		t.Errorf("group missing chip %d (%q); got:\n%s", n, want, group)
	}
	wantOption := `data-option="` + key + `"`
	if !strings.Contains(group, wantOption) {
		t.Errorf("group missing chip option %q; got:\n%s", wantOption, group)
	}
}

// assertFreeReply fails the test unless group contains a reply input wired
// to ticketAttr, the console.js postDraft contract every kind carries.
func assertFreeReply(t *testing.T, group, ticketAttr string) {
	t.Helper()
	if !strings.Contains(group, `class="reply-input"`) {
		t.Errorf("group missing the free reply input; got:\n%s", group)
	}
	if !strings.Contains(group, ticketAttr) {
		t.Errorf("group's reply input missing %q; got:\n%s", ticketAttr, group)
	}
}

// assertItemRow fails the test unless group contains an item row for ref,
// carrying its ref and all four closed-set decision controls.
func assertItemRow(t *testing.T, group, ref string) {
	t.Helper()
	if !strings.Contains(group, `data-item-ref="`+ref+`"`) {
		t.Errorf("group missing item row for ref %q; got:\n%s", ref, group)
	}
	for _, decision := range []string{"accept", "reject", "drop", "discuss"} {
		want := `data-item-ref="` + ref + `" data-decision="` + decision + `"`
		if !strings.Contains(group, want) {
			t.Errorf("item row %q missing its %q control (%q); got:\n%s", ref, decision, want, group)
		}
	}
}

// assertNoItemControls fails the test if group contains any item-decision
// control, proving an option kind never renders the item controls.
func assertNoItemControls(t *testing.T, group string) {
	t.Helper()
	if strings.Contains(group, `class="items"`) {
		t.Errorf("option kind must not render item rows; got:\n%s", group)
	}
}
