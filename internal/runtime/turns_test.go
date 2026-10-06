package runtime

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// turnLine builds one transcript JSONL line. messageID is "" for a
// non-assistant line (the type becomes "user" instead of "assistant").
func turnLine(ts time.Time, messageID string, outputTokens int) string {
	if messageID == "" {
		return `{"type":"user","timestamp":"` + ts.Format(time.RFC3339Nano) + `"}`
	}
	return `{"type":"assistant","timestamp":"` + ts.Format(time.RFC3339Nano) +
		`","message":{"id":"` + messageID + `","usage":{"output_tokens":` + itoa(outputTokens) + `}}}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestLongTurns covers the turn timing rule (design shape): the keep step
// (at least 60 s, longest first, capped at 3), a message split over two
// lines, lines that must be skipped, a leading assistant line with no
// turn, and a reader whose error stops parsing without losing turns
// already read.
func TestLongTurns(t *testing.T) {
	t.Parallel()

	t.Run("keeps the 3 longest at or over 60s", func(t *testing.T) {
		t.Parallel()
		base := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
		var b strings.Builder
		// turn durations, each immediately preceded by a user line that
		// serves as the next turn's start marker (the turn timing rule: a
		// turn's start is the timestamp of the line before its first
		// assistant line).
		b.WriteString(turnLine(base, "", 0) + "\n")
		b.WriteString(turnLine(base.Add(59*time.Second), "msg59", 1) + "\n")
		b.WriteString(turnLine(base.Add(60*time.Second), "", 0) + "\n")
		b.WriteString(turnLine(base.Add(120*time.Second), "msg60", 2) + "\n")
		b.WriteString(turnLine(base.Add(121*time.Second), "", 0) + "\n")
		b.WriteString(turnLine(base.Add(182*time.Second), "msg61", 3) + "\n")
		b.WriteString(turnLine(base.Add(183*time.Second), "", 0) + "\n")
		b.WriteString(turnLine(base.Add(303*time.Second), "msg120", 4) + "\n")
		b.WriteString(turnLine(base.Add(304*time.Second), "", 0) + "\n")
		b.WriteString(turnLine(base.Add(704*time.Second), "msg400", 5) + "\n")

		got, err := longTurns(strings.NewReader(b.String()))
		if err != nil {
			t.Fatalf("longTurns: %v", err)
		}
		wantSeconds := []int{400, 120, 61}
		if len(got) != len(wantSeconds) {
			t.Fatalf("got %d turns, want %d: %+v", len(got), len(wantSeconds), got)
		}
		for i, want := range wantSeconds {
			if got[i].Seconds != want {
				t.Errorf("turn %d: Seconds = %d, want %d", i, got[i].Seconds, want)
			}
		}
	})

	t.Run("message split over two lines times to its last line and larger tokens", func(t *testing.T) {
		t.Parallel()
		base := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
		input := turnLine(base, "", 0) + "\n" +
			turnLine(base.Add(65*time.Second), "msgX", 5) + "\n" +
			turnLine(base.Add(70*time.Second), "msgX", 100) + "\n"

		got, err := longTurns(strings.NewReader(input))
		if err != nil {
			t.Fatalf("longTurns: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d turns, want 1: %+v", len(got), got)
		}
		if got[0].Seconds != 70 {
			t.Errorf("Seconds = %d, want 70", got[0].Seconds)
		}
		if got[0].OutputTokens != 100 {
			t.Errorf("OutputTokens = %d, want 100 (the larger of the two lines)", got[0].OutputTokens)
		}
		if !got[0].Start.Equal(base) {
			t.Errorf("Start = %v, want %v", got[0].Start, base)
		}
	})

	t.Run("malformed lines and lines without a timestamp are skipped", func(t *testing.T) {
		t.Parallel()
		base := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
		input := turnLine(base, "", 0) + "\n" +
			"not json at all\n" +
			`{"type":"user"}` + "\n" + // no timestamp field
			turnLine(base.Add(90*time.Second), "msgY", 7) + "\n"

		got, err := longTurns(strings.NewReader(input))
		if err != nil {
			t.Fatalf("longTurns: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d turns, want 1: %+v", len(got), got)
		}
		if got[0].Seconds != 90 {
			t.Errorf("Seconds = %d, want 90", got[0].Seconds)
		}
		if !got[0].Start.Equal(base) {
			t.Errorf("Start = %v, want %v (the skipped lines must not become the start)", got[0].Start, base)
		}
	})

	t.Run("a leading assistant line gives no turn", func(t *testing.T) {
		t.Parallel()
		base := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
		input := turnLine(base, "msgFirst", 9) + "\n"

		got, err := longTurns(strings.NewReader(input))
		if err != nil {
			t.Fatalf("longTurns: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d turns, want 0: %+v", len(got), got)
		}
	})

	t.Run("a non-EOF read error returns the turns read so far, with the error", func(t *testing.T) {
		t.Parallel()
		base := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
		body := turnLine(base, "", 0) + "\n" +
			turnLine(base.Add(300*time.Second), "msgZ", 11) + "\n"
		wantErr := errors.New("boom")
		r := &errAfterReader{r: strings.NewReader(body), err: wantErr}

		got, err := longTurns(r)
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
		if len(got) != 1 {
			t.Fatalf("got %d turns, want 1: %+v", len(got), got)
		}
		if got[0].Seconds != 300 {
			t.Errorf("Seconds = %d, want 300", got[0].Seconds)
		}
	})
}

// errAfterReader reads everything r has, then returns err instead of
// io.EOF, for TestLongTurns' read-failure case.
type errAfterReader struct {
	r   io.Reader
	err error
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		return n, e.err
	}
	return n, err
}
