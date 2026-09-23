package console_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"

	"zing/internal/console"
)

// newTestHandler builds a console.Handler over a fresh buffer, seeded at
// info, and returns both so a test can assert on the sink's own text output
// as well as the ring.
func newTestHandler(t *testing.T) (*console.Handler, *bytes.Buffer, *slog.LevelVar) {
	t.Helper()
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)
	return console.NewHandler(&buf, lv), &buf, lv
}

func TestLevelVarSetChangesLevelLive(t *testing.T) {
	h, buf, lv := newTestHandler(t)
	logger := slog.New(h)

	logger.Debug("below the sill")
	if buf.Len() != 0 {
		t.Fatalf("before Set(Debug): sink = %q, want empty (info gates out debug)", buf.String())
	}
	if got := h.Tail(0); len(got) != 0 {
		t.Fatalf("before Set(Debug): Tail(0) = %v, want empty", got)
	}

	lv.Set(slog.LevelDebug)
	logger.Debug("now at the sill", "run_id", int64(0))
	if buf.Len() == 0 {
		t.Fatal("after Set(Debug): sink is still empty, want the debug line emitted")
	}
	if got := h.Tail(0); len(got) != 1 {
		t.Fatalf("after Set(Debug): Tail(0) = %v, want exactly one entry", got)
	}
}

func TestPerTicketDebugRaisesLevel_DirectAttribute(t *testing.T) {
	h, buf, _ := newTestHandler(t) // levelVar stays at info
	logger := slog.New(h)
	const ticketID = int64(42)

	logger.Debug("not yet debugged", "ticket_id", ticketID)
	if buf.Len() != 0 {
		t.Fatalf("before SetDebug: sink = %q, want empty", buf.String())
	}

	h.SetDebug(ticketID, true)
	logger.Debug("debugged via a direct attribute", "ticket_id", ticketID)
	if !strings.Contains(buf.String(), "debugged via a direct attribute") {
		t.Errorf("after SetDebug: sink = %q, want the debug line emitted", buf.String())
	}

	buf.Reset()
	logger.Debug("a different ticket stays gated", "ticket_id", ticketID+1)
	if buf.Len() != 0 {
		t.Errorf("a debug line for an undebugged ticket = %q, want it dropped", buf.String())
	}
}

func TestPerTicketDebugRaisesLevel_LoggerWith(t *testing.T) {
	h, buf, _ := newTestHandler(t)
	const ticketID = int64(7)
	h.SetDebug(ticketID, true)

	logger := slog.New(h).With("ticket_id", ticketID)
	logger.Debug("debugged via logger.With")

	if !strings.Contains(buf.String(), "debugged via logger.With") {
		t.Errorf("sink = %q, want the debug line emitted", buf.String())
	}
}

func TestPerTicketDebugRaisesLevel_ChildHandler(t *testing.T) {
	h, buf, _ := newTestHandler(t)
	const ticketID = int64(99)
	h.SetDebug(ticketID, true)

	// A child obtained straight from slog.Handler.WithAttrs, bypassing
	// logger.With, then wrapped in its own fresh Logger: this exercises the
	// Handler interface's own WithAttrs return value directly, distinct
	// from going through the top-level logger (TestPerTicketDebugRaisesLevel_LoggerWith).
	child := h.WithAttrs([]slog.Attr{slog.Int64("ticket_id", ticketID)})
	slog.New(child).Debug("debugged via a child handler")

	if !strings.Contains(buf.String(), "debugged via a child handler") {
		t.Errorf("sink = %q, want the debug line emitted", buf.String())
	}
}

// TestPerTicketDebugThroughWithGroup proves ticket_id still resolves when
// WithAttrs is called on a handler that already has an open WithGroup, so a
// caller elsewhere in the app grouping unrelated fields does not silently
// blind the per-ticket override (design section 6.12: "resolves ticket_id
// ... from attributes accumulated through WithAttrs and WithGroup").
func TestPerTicketDebugThroughWithGroup(t *testing.T) {
	h, buf, _ := newTestHandler(t)
	const ticketID = int64(13)
	h.SetDebug(ticketID, true)

	grouped := h.WithGroup("run").WithAttrs([]slog.Attr{slog.Int64("ticket_id", ticketID)})
	slog.New(grouped).Debug("debugged through an open group")

	if !strings.Contains(buf.String(), "debugged through an open group") {
		t.Errorf("sink = %q, want the debug line emitted", buf.String())
	}
}

func TestRingKeepsLastNAndFiltersByRunID(t *testing.T) {
	h, _, lv := newTestHandler(t)
	lv.Set(slog.LevelDebug)
	logger := slog.New(h)

	const total = console.RingCapacity + 5
	for i := range total {
		logger.Debug(fmt.Sprintf("line %d", i), "run_id", int64(i%2))
	}

	all := append(h.Tail(0), h.Tail(1)...)
	if len(all) != console.RingCapacity {
		t.Fatalf("ring holds %d entries across run_id 0 and 1, want exactly %d (RingCapacity)", len(all), console.RingCapacity)
	}

	// The oldest 5 lines (run_id alternates 0,1,0,1,0 starting at i=0) were
	// evicted, so line 0 (run_id 0) must be gone from Tail(0) but a later
	// line with run_id 0 must remain.
	got0 := h.Tail(0)
	for _, e := range got0 {
		if e.Message == "line 0" {
			t.Errorf("Tail(0) still contains the oldest evicted entry %q", e.Message)
		}
	}
	if len(got0) == 0 {
		t.Fatal("Tail(0) is empty, want the surviving run_id=0 entries")
	}
}

func TestFencedAttrLogsLengthAndHashNotRaw(t *testing.T) {
	h, buf, lv := newTestHandler(t)
	lv.Set(slog.LevelDebug)
	logger := slog.New(h)

	const secret = "sk-super-secret-token-do-not-log"
	logger.Debug("body received", console.FencedAttr("body", secret))

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("sink = %q, must never contain the raw fenced value", out)
	}
	if !strings.Contains(out, "len="+strconv.Itoa(len(secret))) {
		t.Errorf("sink = %q, want the fenced value's length (%d)", out, len(secret))
	}
	if !strings.Contains(out, "sha256=") {
		t.Errorf("sink = %q, want a sha256= field", out)
	}
}

// TestConcurrentTogglesAreSafe exercises SetDebug, LevelVar.Set, and Handle
// from many goroutines at once; run under `go test -race` (design section
// 6.12, 11: "concurrent toggles are safe").
func TestConcurrentTogglesAreSafe(t *testing.T) {
	h, _, lv := newTestHandler(t)
	logger := slog.New(h)

	const goroutines = 8
	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	for g := range goroutines {
		ticketID := int64(g)
		go func() {
			defer wg.Done()
			for range iterations {
				h.SetDebug(ticketID, true)
				h.SetDebug(ticketID, false)
			}
		}()
		go func() {
			defer wg.Done()
			for range iterations {
				if lv.Level() == slog.LevelDebug {
					lv.Set(slog.LevelInfo)
				} else {
					lv.Set(slog.LevelDebug)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range iterations {
				logger.Debug("concurrent", "ticket_id", ticketID, "run_id", ticketID)
			}
		}()
	}
	wg.Wait()

	_ = h.Tail(0) // reads the ring after the race, proving it is still consistent
}
