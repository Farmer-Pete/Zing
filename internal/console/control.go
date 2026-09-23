// control.go is POST /loglevel and POST /debug (design section 6.12, 7.1):
// the two endpoints Task 5's log.go left for this task to wire in. Both
// mutate the same *Handler cmd/zing installed as slog's default (design
// section 6.12, cmd/zing/serve.go's installLogHandler) and sit behind the
// mutation guard like every other state-changing route (mw.go).
package console

import (
	"log/slog"
	"net/http"
	"sync"
)

// logLevelsByName is the design section 6.12 closed set POST /loglevel
// accepts: "debug/info/warn/error", named exactly that way, not slog's own
// "WARN"/"ERROR" spelling.
var logLevelsByName = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// ParseLogLevel maps name to its slog.Level when name is one of the design
// section 6.12 closed set; ok is false for anything else. Exported so
// cmd/zing's installLogHandler (serve.go, seeding the level from
// settings.log_level at startup) and handleLogLevel below share one
// closed-set definition instead of two.
func ParseLogLevel(name string) (level slog.Level, ok bool) {
	level, ok = logLevelsByName[name]
	return level, ok
}

// settingLogLevel is the settings.key POST /loglevel writes (design section
// 6.12: "writes settings.log_level"), the same key migrations/0001_init.sql
// seeds and cmd/zing's installLogHandler reads at startup.
const settingLogLevel = "log_level"

// logLevelRequest is POST /loglevel's body (design section 7.1: "the
// level").
type logLevelRequest struct {
	Level string `json:"level"`
}

// logLevelMu serializes POST /loglevel's write to settings.log_level with
// the live LevelVar.Set call right after it (review fix, PR #16: without
// this, two concurrent requests can interleave their write-then-set pairs
// -- request A's SetSettings, then B's SetSettings, then B's SetLevel, then
// A's SetLevel -- leaving the persisted setting and the live level
// disagreeing until the next successful POST /loglevel or process
// restart). Package-level rather than a field on *console: this critical
// section is the only place either half of that pair is touched, and one
// zing process runs one console.
var logLevelMu sync.Mutex

// handleLogLevel is POST /loglevel (design section 6.12, 7.1): validate the
// requested level against the closed set, then, under logLevelMu, write
// settings.log_level and call LevelVar.Set through the handler so the
// change takes hold at once, before this request even returns. 400 on a
// level outside the closed set (or a malformed body), 204 and a bus
// publish on success.
func (c *console) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDraftBodyBytes)

	var req logLevelRequest
	if err := decodeStrict(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	level, ok := ParseLogLevel(req.Level)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	logLevelMu.Lock()
	defer logLevelMu.Unlock()

	if err := c.store.SetSettings(r.Context(), settingLogLevel, req.Level); err != nil {
		slog.Error("console: set log level", "level", req.Level, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}
	c.log.SetLevel(level)

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
}

// debugRequest is POST /debug's body (design section 7.1: "the ticket id").
type debugRequest struct {
	Ticket int64 `json:"ticket"`
}

// handleDebug is POST /debug (design section 6.12, 7.1): toggle the
// per-ticket debug override for the given ticket id, on if it was off and
// off if it was on, through the log Handler's own atomic ToggleDebug so two
// concurrent requests for the same ticket cannot race a
// read-then-write pair into dropping one toggle. 400 on a malformed body or
// non-positive ticket, 204 and a bus publish on success.
func (c *console) handleDebug(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDraftBodyBytes)

	var req debugRequest
	if err := decodeStrict(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if req.Ticket <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	c.log.ToggleDebug(req.Ticket)

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
}
