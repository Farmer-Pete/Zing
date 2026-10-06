package runtime

import (
	"strconv"
	"strings"
	"time"
	// Loaded for its side effect: LoadLocation must resolve IANA zone names
	// (America/New_York, UTC, ...) even inside a build sandbox that denies
	// /usr/share/zoneinfo.
	_ "time/tzdata"
)

// sessionLimitPrefix is the exact text Claude's final message starts with
// when its session usage limit is hit (owner decision Q4: session limit
// only, no other wording).
const sessionLimitPrefix = "You've hit your session limit"

// sessionLimitFallback is the park duration used when the reset text after
// sessionLimitPrefix does not parse.
const sessionLimitFallback = 30 * time.Minute

// SessionLimitError reports a Claude run that exited non-zero because its
// session usage limit was hit (design section "Session-limit message
// grammar"). ResetAt is the next instant, after the now passed to
// parseSessionLimit, that the message's reset text names; Parsed is false
// when that text did not parse, in which case ResetAt is now plus 30
// minutes.
type SessionLimitError struct {
	ResetAt time.Time
	Parsed  bool
}

func (e *SessionLimitError) Error() string {
	return "runtime: claude session limit, resets " + e.ResetAt.UTC().Format(time.RFC3339)
}

// parseSessionLimit reports whether msg, untrimmed, starts with Claude's
// session-limit prefix and, when it does, the reset instant after now
// (Parsed false and now plus 30 minutes when the reset text does not
// parse).
func parseSessionLimit(msg string, now time.Time) (*SessionLimitError, bool) {
	if !strings.HasPrefix(msg, sessionLimitPrefix) {
		return nil, false
	}

	resetAt, parsed := now.Add(sessionLimitFallback), false
	const marker = "resets "
	if idx := strings.LastIndex(msg, marker); idx != -1 {
		if t, ok := parseResetTime(msg[idx+len(marker):], now); ok {
			resetAt, parsed = t, true
		}
	}
	return &SessionLimitError{ResetAt: resetAt, Parsed: parsed}, true
}

// parseResetTime parses s per the reset grammar (design section
// "Session-limit message grammar") and applies the next-instant rule: the
// first instant after now at that wall-clock time in the named zone.
func parseResetTime(s string, now time.Time) (time.Time, bool) {
	sc := &resetScanner{s: s}

	hour, ok := sc.hour()
	if !ok {
		return time.Time{}, false
	}
	minute := 0
	if sc.i < len(sc.s) && sc.s[sc.i] == ':' {
		sc.i++
		minute, ok = sc.minute()
		if !ok {
			return time.Time{}, false
		}
	}
	pm, ok := sc.ampm()
	if !ok {
		return time.Time{}, false
	}
	zoneName, ok := sc.zone()
	if !ok {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(zoneName)
	if err != nil {
		return time.Time{}, false
	}

	h24 := hour % 12
	if pm {
		h24 += 12
	}

	n := now.In(loc)
	c := time.Date(n.Year(), n.Month(), n.Day(), h24, minute, 0, 0, loc)
	if !c.After(now) {
		c = time.Date(n.Year(), n.Month(), n.Day()+1, h24, minute, 0, 0, loc)
	}
	return c, true
}

// resetScanner is a recursive-descent scanner over s, starting at i: hour,
// minute, ampm and zone each consume from the front and report ok false on
// a rule miss, leaving i wherever the failed rule stopped looking.
type resetScanner struct {
	s string
	i int
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// hour consumes 1 or 2 ASCII digits and reports their value, 1 to 12.
func (r *resetScanner) hour() (int, bool) {
	start := r.i
	for r.i-start < 2 && r.digitAt(0) {
		r.i++
	}
	if r.i == start {
		return 0, false
	}
	h, err := strconv.Atoi(r.s[start:r.i])
	if err != nil || h < 1 || h > 12 {
		return 0, false
	}
	return h, true
}

// digitAt reports whether the byte k past r.i exists and is an ASCII digit.
func (r *resetScanner) digitAt(k int) bool {
	return r.i+k < len(r.s) && isASCIIDigit(r.s[r.i+k])
}

// minute consumes exactly 2 ASCII digits and reports their value, 0 to 59.
func (r *resetScanner) minute() (int, bool) {
	if !r.digitAt(0) || !r.digitAt(1) {
		return 0, false
	}
	m, err := strconv.Atoi(r.s[r.i : r.i+2])
	if err != nil || m > 59 {
		return 0, false
	}
	r.i += 2
	return m, true
}

// ampm consumes "am" or "pm", case-insensitive, and reports whether it was pm.
func (r *resetScanner) ampm() (pm, ok bool) {
	if r.i+2 > len(r.s) {
		return false, false
	}
	switch strings.ToLower(r.s[r.i : r.i+2]) {
	case "am":
		r.i += 2
		return false, true
	case "pm":
		r.i += 2
		return true, true
	default:
		return false, false
	}
}

// zone consumes " (" then 1 or more bytes up to the next ")", which it
// consumes too, and reports the bytes between them.
func (r *resetScanner) zone() (string, bool) {
	if !strings.HasPrefix(r.s[r.i:], " (") {
		return "", false
	}
	start := r.i + 2
	end := strings.IndexByte(r.s[start:], ')')
	if end <= 0 {
		return "", false
	}
	r.i = start + end + 1
	return r.s[start : start+end], true
}

// exitFailure is the error for a process that exited non-zero: a
// SessionLimitError when finalMessage is the session-limit message,
// otherwise the ExecError every runtime returned before.
func exitFailure(exitCode int, finalMessage string, now time.Time) error {
	if sl, ok := parseSessionLimit(finalMessage, now); ok {
		return sl
	}
	return &ExecError{ExitCode: exitCode}
}
