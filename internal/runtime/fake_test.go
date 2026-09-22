package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/fstest"

	"zing/internal/response"
)

const (
	classifyBugXML     = `<zing job="classify" outcome="bug"><reason>first turn</reason></zing>`
	classifyFeatureXML = `<zing job="classify" outcome="feature"><reason>second turn</reason></zing>`
	brokenXML          = `not a zing document`
)

func newClassifyFS() fstest.MapFS {
	return fstest.MapFS{
		"classify/1.xml":        &fstest.MapFile{Data: []byte(classifyBugXML)},
		"classify/2.xml":        &fstest.MapFile{Data: []byte(classifyFeatureXML)},
		"classify/label/1.xml":  &fstest.MapFile{Data: []byte(classifyBugXML)},
		"classify/broken/1.xml": &fstest.MapFile{Data: []byte(brokenXML)},
	}
}

func TestFake_ScriptedTurnReturnsDocument(t *testing.T) {
	t.Parallel()

	f := NewFake(newClassifyFS())
	res, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Response == nil {
		t.Fatal("Run returned a nil Response")
	}
	if got := res.Response.Header().Outcome; got != response.OutcomeBug {
		t.Errorf("Response outcome = %s, want %s", got, response.OutcomeBug)
	}
}

func TestFake_LabelSelectsItsOwnScript(t *testing.T) {
	t.Parallel()

	f := NewFake(newClassifyFS())
	res, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify, Label: "label"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := res.Response.Header().Outcome; got != response.OutcomeBug {
		t.Errorf("Response outcome = %s, want %s", got, response.OutcomeBug)
	}
}

func TestFake_FirstTurnMintsIDAndServesTurnOne(t *testing.T) {
	t.Parallel()

	f := NewFake(newClassifyFS())
	res, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SessionID == "" {
		t.Fatal("Run returned an empty SessionID")
	}
}

func TestFake_HonorsCancellation(t *testing.T) {
	t.Parallel()

	f := NewFake(newClassifyFS())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.Run(ctx, RunRequest{Job: response.JobClassify})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run with a cancelled context = %v, want context.Canceled", err)
	}
	if len(f.sessions) != 0 {
		t.Errorf("a cancelled Run left %d sessions; want 0 (no turn consumed)", len(f.sessions))
	}
}

func TestFake_FailedFirstTurnLeavesNoSession(t *testing.T) {
	t.Parallel()

	f := NewFake(newClassifyFS())
	// classify/broken/1.xml holds a non-zing document, so the first turn's
	// parse fails; the minted session must not be committed to the map.
	_, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify, Label: "broken"})
	if err == nil {
		t.Fatal("Run on a broken first-turn script returned a nil error")
	}
	if len(f.sessions) != 0 {
		t.Errorf("a failed first turn left %d sessions; want 0", len(f.sessions))
	}
}

func TestFake_TwoInstancesMintDistinctIDs(t *testing.T) {
	t.Parallel()

	f1, f2 := NewFake(newClassifyFS()), NewFake(newClassifyFS())
	ctx := context.Background()

	res1, err := f1.Run(ctx, RunRequest{Job: response.JobClassify})
	if err != nil {
		t.Fatalf("f1.Run: %v", err)
	}
	res2, err := f2.Run(ctx, RunRequest{Job: response.JobClassify})
	if err != nil {
		t.Fatalf("f2.Run: %v", err)
	}
	if res1.SessionID == res2.SessionID {
		t.Errorf("two Fake instances minted the same session id %s", res1.SessionID)
	}
}

func TestFake_ResumeAdvancesTurn(t *testing.T) {
	t.Parallel()

	f := NewFake(newClassifyFS())
	ctx := context.Background()

	res1, err := f.Run(ctx, RunRequest{Job: response.JobClassify})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}

	res2, err := f.Run(ctx, RunRequest{Job: response.JobClassify, SessionID: res1.SessionID})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if res2.SessionID != res1.SessionID {
		t.Errorf("resumed SessionID = %s, want %s", res2.SessionID, res1.SessionID)
	}
	if got := res2.Response.Header().Outcome; got != response.OutcomeFeature {
		t.Errorf("turn 2 outcome = %s, want %s", got, response.OutcomeFeature)
	}
}

func TestFake_UnknownSessionErrors(t *testing.T) {
	t.Parallel()

	f := NewFake(newClassifyFS())
	_, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify, SessionID: "fake-9999"})
	if err == nil {
		t.Fatal("Run with an unknown session id, want an error")
	}
}

func TestFake_MismatchedResumeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  func(sessionID string) RunRequest
	}{
		{"job differs", func(id string) RunRequest {
			return RunRequest{Job: response.JobPlanning, Label: "", SessionID: id}
		}},
		{"label differs", func(id string) RunRequest {
			return RunRequest{Job: response.JobClassify, Label: "label", SessionID: id}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := NewFake(newClassifyFS())
			ctx := context.Background()

			res1, err := f.Run(ctx, RunRequest{Job: response.JobClassify})
			if err != nil {
				t.Fatalf("turn 1: %v", err)
			}

			if _, err := f.Run(ctx, tt.req(res1.SessionID)); err == nil {
				t.Fatal("resume with a mismatched (job, label), want an error")
			}
		})
	}
}

func TestFake_MissingScriptErrors(t *testing.T) {
	t.Parallel()

	f := NewFake(newClassifyFS())
	_, err := f.Run(context.Background(), RunRequest{Job: response.Job("nope")})
	want := "fake: no script for nope/1.xml"
	if err == nil || err.Error() != want {
		t.Fatalf("Run error = %v, want %q", err, want)
	}
}

// TestFake_BrokenScriptDoesNotAdvanceTurn scripts a MALFORMED turn 2 (not
// merely a missing one, which Run would fail identically whether or not it
// had already advanced): the malformed script proves the failure happens
// before nextTurn moves past 2, because the retry below, with turn 2
// replaced by valid XML, must still serve turn 2's content rather than
// turn 3's (there is no turn 3 script at all, so an advance-then-fail bug
// here would make the retry error with a missing-script message, not
// succeed with the feature outcome below).
func TestFake_BrokenScriptDoesNotAdvanceTurn(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"classify/1.xml": &fstest.MapFile{Data: []byte(classifyBugXML)},
		"classify/2.xml": &fstest.MapFile{Data: []byte(brokenXML)},
	}
	f := NewFake(fsys)
	ctx := context.Background()

	res1, err := f.Run(ctx, RunRequest{Job: response.JobClassify})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}

	// Turn 2's script is malformed XML: response.Parse must fail on it,
	// and that failure must happen before the turn advances.
	if _, malformedErr := f.Run(ctx, RunRequest{Job: response.JobClassify, SessionID: res1.SessionID}); malformedErr == nil {
		t.Fatal("Run with a malformed turn 2 script, want an error")
	}

	// The script is replaced with valid XML; the resumed session must
	// still serve turn 2, not turn 3, proving the failed attempt above
	// never advanced nextTurn.
	fsys["classify/2.xml"] = &fstest.MapFile{Data: []byte(classifyFeatureXML)}
	res2, err := f.Run(ctx, RunRequest{Job: response.JobClassify, SessionID: res1.SessionID})
	if err != nil {
		t.Fatalf("retried turn 2: %v", err)
	}
	if got := res2.Response.Header().Outcome; got != response.OutcomeFeature {
		t.Errorf("retried turn 2 outcome = %s, want %s", got, response.OutcomeFeature)
	}
}

// TestFake_ConcurrentSessionsRace drives many concurrent first turns on one
// shared Fake, so `go test -race` exercises the mutex guarding its session
// map and the package-level id counter.
func TestFake_ConcurrentSessionsRace(t *testing.T) {
	t.Parallel()

	f := NewFake(newClassifyFS())
	ctx := context.Background()

	const n = 20
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := f.Run(ctx, RunRequest{Job: response.JobClassify})
			if err != nil {
				t.Errorf("Run: %v", err)
				return
			}
			ids[i] = res.SessionID
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for _, id := range ids {
		if id == "" {
			t.Fatal("a concurrent Run returned an empty session id")
		}
		if seen[id] {
			t.Fatalf("duplicate session id %s minted under concurrency", id)
		}
		seen[id] = true
	}
}
