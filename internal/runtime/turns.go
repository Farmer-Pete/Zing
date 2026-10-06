package runtime

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"os"
	"sort"
	"time"
)

// longTurnMinSeconds and longTurnMax are the long-turn path's own
// thresholds (owner Q3): only turns of at least this many seconds are
// kept, and at most this many, longest first, are logged per run.
const (
	longTurnMinSeconds = 60
	longTurnMax        = 3
)

// longTurn is one model turn from a Claude transcript, per the turn timing
// rule (design shape): Start is the timestamp of the line before the
// turn's first assistant line, Seconds is the turn's end minus Start
// rounded to the nearest whole second, and OutputTokens is the largest
// usage.output_tokens seen on any of the turn's lines.
type longTurn struct {
	Start        time.Time
	Seconds      int
	OutputTokens int
}

// transcriptLine is the subset of one Claude transcript JSONL line longTurns
// reads: every other field (uuid, parentUuid, cwd, ...) is ignored.
type transcriptLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID    string `json:"id"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// parsedLine is one transcript JSONL line, decoded and timestamped.
type parsedLine struct {
	Time time.Time
	Line transcriptLine
}

// parseTranscriptLine decodes one raw JSONL line and parses its timestamp;
// ok is false when either fails, per the turn timing rule ("skip any line
// that does not decode or whose timestamp does not parse as RFC 3339").
func parseTranscriptLine(raw []byte) (parsedLine, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return parsedLine{}, false
	}
	var tl transcriptLine
	if err := json.Unmarshal(raw, &tl); err != nil {
		return parsedLine{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, tl.Timestamp)
	if err != nil {
		return parsedLine{}, false
	}
	return parsedLine{Time: ts, Line: tl}, true
}

// openTurn is a turn longTurns is still accumulating lines for.
type openTurn struct {
	id     string
	start  time.Time
	end    time.Time
	tokens int
}

// extend grows t with another line of the same message id: end moves to
// ts, and tokens becomes the larger of its current value and tokens.
func (t *openTurn) extend(ts time.Time, tokens int) {
	t.end = ts
	if tokens > t.tokens {
		t.tokens = tokens
	}
}

// finalize turns t into the longTurn it represents, per the turn timing
// rule: seconds is end minus start, rounded to the nearest whole second.
func (t *openTurn) finalize() longTurn {
	return longTurn{
		Start:        t.start,
		Seconds:      int(math.Round(t.end.Sub(t.start).Seconds())),
		OutputTokens: t.tokens,
	}
}

// flushInto appends t's finalized turn to out, unless t is nil or has no
// start: a turn with no previous line is dropped, per the turn timing
// rule. longTurns calls this both mid-stream, when a new assistant message
// id starts a turn, and at the end of the transcript.
func (t *openTurn) flushInto(out []longTurn) []longTurn {
	if t == nil || t.start.IsZero() {
		return out
	}
	return append(out, t.finalize())
}

// longTurns reads a Claude transcript line by line and returns up to
// longTurnMax turns of at least longTurnMinSeconds, longest first (ties by
// start ascending), per the turn timing rule. A read error other than
// io.EOF stops parsing; the turns read so far are still returned, filtered
// and capped the same way, alongside the error.
func longTurns(r io.Reader) ([]longTurn, error) {
	br := bufio.NewReader(r)
	var finalized []longTurn
	var current *openTurn
	var prevTime time.Time

	for {
		raw, readErr := br.ReadBytes('\n')
		pl, ok := parseTranscriptLine(raw)
		switch {
		case !ok:
			// skip: empty, undecodable, or an unparsable timestamp
		case pl.Line.Type != "assistant" || pl.Line.Message.ID == "":
			prevTime = pl.Time
		case current != nil && current.id == pl.Line.Message.ID:
			current.extend(pl.Time, pl.Line.Message.Usage.OutputTokens)
			prevTime = pl.Time
		default:
			finalized = current.flushInto(finalized)
			current = &openTurn{id: pl.Line.Message.ID, start: prevTime, end: pl.Time, tokens: pl.Line.Message.Usage.OutputTokens}
			prevTime = pl.Time
		}
		if readErr != nil {
			finalized = current.flushInto(finalized)
			if readErr == io.EOF {
				readErr = nil
			}
			return keepLongTurns(finalized), readErr
		}
	}
}

// keepLongTurns applies the turn timing rule's keep step: turns of at least
// longTurnMinSeconds, sorted longest first (ties by start ascending), capped
// at longTurnMax.
func keepLongTurns(turns []longTurn) []longTurn {
	var kept []longTurn
	for _, t := range turns {
		if t.Seconds >= longTurnMinSeconds {
			kept = append(kept, t)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Seconds != kept[j].Seconds {
			return kept[i].Seconds > kept[j].Seconds
		}
		return kept[i].Start.Before(kept[j].Start)
	})
	if len(kept) > longTurnMax {
		kept = kept[:longTurnMax]
	}
	return kept
}

// claudeLongTurnLogMsg is the INFO log message logLongTurns writes for
// each kept turn, named once so claude_test.go's own assertions share it
// rather than repeating the literal (goconst).
const claudeLongTurnLogMsg = "claude long turn"

// logLongTurns logs a run's longest model turns (design shape, the
// long-turn path): one claudeLongTurnLogMsg INFO record per kept turn, or
// a single DEBUG record when the transcript is missing or only partly
// readable. It never changes the run's result; it only logs. Every record
// carries job and run_id (req.RunToken).
func logLongTurns(req RunRequest, path string) {
	if path == "" {
		slog.Debug("claude long turns: no transcript", "job", req.Job, "run_id", req.RunToken)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		slog.Debug("claude long turns: no transcript", "job", req.Job, "run_id", req.RunToken, "error", err)
		return
	}
	defer f.Close()

	turns, err := longTurns(f)
	if err != nil {
		slog.Debug("claude long turns: transcript read failed",
			"job", req.Job, "run_id", req.RunToken, "turns_kept", len(turns), "error", err)
	}
	for i, t := range turns {
		slog.Info(claudeLongTurnLogMsg,
			"job", req.Job,
			"run_id", req.RunToken,
			"rank", i+1,
			"started_at", t.Start,
			"seconds", t.Seconds,
			"output_tokens", t.OutputTokens,
		)
	}
}
