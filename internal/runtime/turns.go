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

// parseTranscriptLine decodes one raw JSONL line and parses its timestamp;
// ok is false when either fails, per the turn timing rule ("skip any line
// that does not decode or whose timestamp does not parse as RFC 3339").
func parseTranscriptLine(raw []byte) (ts time.Time, isAssistant bool, messageID string, outputTokens int, ok bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return time.Time{}, false, "", 0, false
	}
	var tl transcriptLine
	if err := json.Unmarshal(raw, &tl); err != nil {
		return time.Time{}, false, "", 0, false
	}
	ts, err := time.Parse(time.RFC3339Nano, tl.Timestamp)
	if err != nil {
		return time.Time{}, false, "", 0, false
	}
	return ts, tl.Type == "assistant", tl.Message.ID, tl.Message.Usage.OutputTokens, true
}

// openTurn is a turn longTurns is still accumulating lines for.
type openTurn struct {
	id     string
	start  time.Time
	end    time.Time
	tokens int
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
		line, readErr := br.ReadBytes('\n')
		if len(line) > 0 {
			if ts, isAssistant, messageID, tokens, ok := parseTranscriptLine(line); ok {
				if isAssistant && messageID != "" {
					if current == nil || current.id != messageID {
						if current != nil && !current.start.IsZero() {
							finalized = append(finalized, current.finalize())
						}
						current = &openTurn{id: messageID, start: prevTime, end: ts, tokens: tokens}
					} else {
						current.end = ts
						if tokens > current.tokens {
							current.tokens = tokens
						}
					}
				}
				prevTime = ts
			}
		}
		if readErr != nil {
			if current != nil && !current.start.IsZero() {
				finalized = append(finalized, current.finalize())
			}
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

// logLongTurns logs a run's longest model turns (design shape, the
// long-turn path): one INFO "claude long turn" record per kept turn, or a
// single DEBUG record when the transcript is missing or only partly
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
		slog.Info("claude long turn",
			"job", req.Job,
			"run_id", req.RunToken,
			"rank", i+1,
			"started_at", t.Start,
			"seconds", t.Seconds,
			"output_tokens", t.OutputTokens,
		)
	}
}
