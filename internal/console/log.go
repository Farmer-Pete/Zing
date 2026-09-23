// log.go is the slog.Handler design section 6.12 describes: a text sink and
// an in-memory ring buffer, both accepting every record at debug so nothing
// is dropped before this handler decides, gated by a live *slog.LevelVar
// plus a per-ticket debug override that raises the level for one ticket's
// lines regardless of the global setting. cmd/zing seeds the LevelVar from
// settings.log_level and installs the handler as slog's default (design
// section 6.12, 14); that wiring, and the /loglevel and /debug endpoints
// that mutate it live, are Task 10's (control.go). This file builds and
// tests the handler on its own.
package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// RingCapacity is the ring buffer's fixed size (design section 6.12: "a
// fixed-size slice, capacity 500"). Exported so callers (tests here, and
// the Task 9/10 rail) size their own expectations off the one constant
// rather than a repeated literal.
const RingCapacity = 500

// LogEntry is one ring-buffer row: enough to render the Task 9/10 log rail
// (design section 6.11, "the ring buffer ... filtered by the open ticket's
// run_id") without holding the full slog.Record, whose Attrs are only valid
// during Handle.
type LogEntry struct {
	Time     time.Time
	Level    slog.Level
	Message  string
	TicketID *int64
	RunID    *int64
	TaskN    *int64
}

// Handler is the design section 6.12 slog.Handler: it wraps a text sink and
// a ring, both effectively at debug, behind a live level and a per-ticket
// debug override. The zero value is not usable; construct one with
// NewHandler.
type Handler struct {
	levelVar *slog.LevelVar
	sink     slog.Handler
	ring     *logRing
	debug    *debugSet

	// attrs is this handler node's own flat copy of every attribute
	// accumulated through WithAttrs, used only to resolve ticket_id/run_id/
	// task_n (resolveIDs below). It is deliberately not group-namespaced:
	// WithGroup is still forwarded to sink, so the text sink's own output
	// nests correctly, but a ticket_id attached under a WithGroup call is
	// still found here by a flat key search. The handler only ever needs to
	// answer "does this record carry a ticket_id", never "under which
	// group", so this simpler search is the right depth for what it does.
	attrs []slog.Attr
}

var _ slog.Handler = (*Handler)(nil)

// NewHandler builds a Handler writing its text sink to w, gated by
// levelVar (design section 6.12: "It holds a *slog.LevelVar seeded from
// settings.log_level"; seeding it is the caller's job, since that needs a
// store read this package-level constructor does not take). levelVar must
// not be nil.
func NewHandler(w io.Writer, levelVar *slog.LevelVar) *Handler {
	return &Handler{
		levelVar: levelVar,
		sink:     slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}),
		ring:     newLogRing(RingCapacity),
		debug:    newDebugSet(),
	}
}

// Enabled returns true for any level at or above debug (design section
// 6.12: "Enabled returns true for any level at or above debug, so a
// per-ticket debug record is never filtered before Handle sees it"). The
// real level-or-debug-set decision happens in Handle, which is the only
// place that knows a record's ticket_id.
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelDebug
}

// Handle resolves ticket_id (from the record and from WithAttrs/WithGroup-
// accumulated attrs), emits the record to the sink and the ring when its
// level clears levelVar or its ticket_id is in the debug set, and otherwise
// drops it (design section 6.12).
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	ticketID, runID, taskN := h.resolveIDs(r)

	debugged := ticketID != nil && h.debug.has(*ticketID)
	if r.Level < h.levelVar.Level() && !debugged {
		return nil
	}

	if err := h.sink.Handle(ctx, r); err != nil {
		return fmt.Errorf("console: log handler: forward to sink: %w", err)
	}

	h.ring.add(LogEntry{
		Time:     r.Time,
		Level:    r.Level,
		Message:  r.Message,
		TicketID: ticketID,
		RunID:    runID,
		TaskN:    taskN,
	})
	return nil
}

// WithAttrs returns a child Handler carrying attrs in addition to h's own,
// forwarded to the text sink for correct formatting and kept as this node's
// own flat copy for resolveIDs (design section 6.12: "logger.With(...) and
// child handlers both work").
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := *h
	h2.sink = h.sink.WithAttrs(attrs)
	h2.attrs = make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	h2.attrs = append(h2.attrs, h.attrs...)
	h2.attrs = append(h2.attrs, attrs...)
	return &h2
}

// WithGroup returns a child Handler whose text-sink output nests under
// name; it does not affect resolveIDs (see the attrs field doc comment).
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.sink = h.sink.WithGroup(name)
	return &h2
}

// SetLevel changes the handler's effective level at once (design section
// 6.12, 7.1: "POST /loglevel ... calls LevelVar.Set"). The caller (Task
// 10's POST /loglevel handler, control.go) validates level is one of
// debug/info/warn/error before calling this; SetLevel itself accepts any
// slog.Level, matching slog.LevelVar.Set's own contract.
func (h *Handler) SetLevel(level slog.Level) {
	h.levelVar.Set(level)
}

// SetDebug turns the per-ticket debug override on or off for ticketID
// (design section 6.12; the Task 10 POST /debug handler calls this).
func (h *Handler) SetDebug(ticketID int64, on bool) {
	h.debug.set(ticketID, on)
}

// IsDebug reports whether ticketID currently has the debug override set.
func (h *Handler) IsDebug(ticketID int64) bool {
	return h.debug.has(ticketID)
}

// ToggleDebug flips ticketID's per-ticket debug override under debugSet's
// one lock (design section 6.12, POST /debug: "on if it was off and off if
// it was on") and returns the state after the flip. control.go's
// handleDebug calls this instead of reading IsDebug and then calling
// SetDebug with its negation, which is a check-then-act pair: two
// concurrent toggles for the same ticket can both read the same starting
// state and both write the same ending state, silently dropping one
// toggle. Folding the read and the write into one critical section removes
// that race.
func (h *Handler) ToggleDebug(ticketID int64) (on bool) {
	return h.debug.toggle(ticketID)
}

// Tail returns the ring's entries for one run, oldest first, filtered by
// run_id (design section 6.11: "the ring buffer ... filtered by the open
// ticket's run_ids"). The returned slice is a fresh copy; the caller may
// retain or mutate it freely.
func (h *Handler) Tail(runID int64) []LogEntry {
	return h.ring.byRunID(runID)
}

// resolveIDs finds ticket_id, run_id, and task_n by searching, for each
// key, the record's own attributes first (the most specific source) and
// then this handler node's accumulated attrs.
func (h *Handler) resolveIDs(r slog.Record) (ticketID, runID, taskN *int64) {
	if v, ok := findInt64Attr("ticket_id", h.attrs, r); ok {
		ticketID = &v
	}
	if v, ok := findInt64Attr("run_id", h.attrs, r); ok {
		runID = &v
	}
	if v, ok := findInt64Attr("task_n", h.attrs, r); ok {
		taskN = &v
	}
	return ticketID, runID, taskN
}

// findInt64Attr looks for key among r's own attributes, then among attrs,
// returning the first integer-valued match.
func findInt64Attr(key string, attrs []slog.Attr, r slog.Record) (int64, bool) {
	var found int64
	var ok bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != key {
			return true
		}
		if v, isInt := attrInt64(a); isInt {
			found, ok = v, true
			return false
		}
		return true
	})
	if ok {
		return found, true
	}
	for _, a := range attrs {
		if a.Key != key {
			continue
		}
		if v, isInt := attrInt64(a); isInt {
			return v, true
		}
	}
	return 0, false
}

// attrInt64 extracts an int64 from a's value when it holds one, covering
// every integer kind slog.Int64, slog.Int (and a plain Go int literal
// argument, which slog's own AnyValue maps to KindInt64), and slog.Uint64
// can produce.
func attrInt64(a slog.Attr) (int64, bool) {
	switch a.Value.Kind() {
	case slog.KindInt64:
		return a.Value.Int64(), true
	case slog.KindUint64:
		u := a.Value.Uint64()
		if u > maxInt64AsUint64 {
			return 0, false
		}
		return int64(u), true
	default:
		return 0, false
	}
}

const maxInt64AsUint64 = uint64(1<<63 - 1)

// FencedAttr returns a slog.Attr that logs a body-shaped value (a message
// body, a plan fence, any user- or model-authored text) by length and
// SHA-256 hash instead of by its raw content, per the design's logging
// rule: never log a secret or a fenced body raw (design section 6.12, §16
// of the design document). Every call site that would otherwise log such a
// value must use this instead of slog.String.
func FencedAttr(key, value string) slog.Attr {
	sum := sha256.Sum256([]byte(value))
	return slog.Group(key,
		slog.Int("len", len(value)),
		slog.String("sha256", hex.EncodeToString(sum[:])),
	)
}

// logRing is the fixed-capacity, mutex-guarded ring buffer (design section
// 6.12: "a fixed-size slice, capacity 500, mutex-guarded").
type logRing struct {
	mu       sync.Mutex
	entries  []LogEntry
	capacity int
}

func newLogRing(capacity int) *logRing {
	return &logRing{entries: make([]LogEntry, 0, capacity), capacity: capacity}
}

// add appends e, dropping the oldest entry once the ring is at capacity.
func (r *logRing) add(e LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	if len(r.entries) > r.capacity {
		copy(r.entries, r.entries[1:])
		r.entries = r.entries[:r.capacity]
	}
}

// byRunID returns a fresh copy of every entry whose RunID equals runID,
// oldest first: never the guarded slice itself (golang skill: "Never let a
// map or slice guarded by a mutex escape the critical section").
func (r *logRing) byRunID(runID int64) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, 0, len(r.entries))
	for _, e := range r.entries {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out
}

// debugSet is the mutex-guarded per-ticket debug override (design section
// 6.12: "a mutex-guarded per-ticket debug set").
type debugSet struct {
	mu  sync.RWMutex
	ids map[int64]struct{}
}

func newDebugSet() *debugSet {
	return &debugSet{ids: make(map[int64]struct{})}
}

func (d *debugSet) set(ticketID int64, on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if on {
		d.ids[ticketID] = struct{}{}
		return
	}
	delete(d.ids, ticketID)
}

func (d *debugSet) has(ticketID int64) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.ids[ticketID]
	return ok
}

// toggle flips ticketID's membership under one lock -- on if it was off,
// off if it was on -- and returns the state after the flip, so a caller
// need not pair a has() read with a set() write across two separate
// critical sections.
func (d *debugSet) toggle(ticketID int64) (on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.ids[ticketID]; ok {
		delete(d.ids, ticketID)
		return false
	}
	d.ids[ticketID] = struct{}{}
	return true
}
