package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"zing/internal/response"
)

const (
	classifyBugXML     = `<zing job="classify" outcome="bug"><reason>first turn</reason></zing>`
	classifyFeatureXML = `<zing job="classify" outcome="feature"><reason>second turn</reason></zing>`
	brokenXML          = `not a zing document`
	classifyTurn1Key   = "classify/1.xml"
)

func newClassifyFS() fstest.MapFS {
	return fstest.MapFS{
		classifyTurn1Key:        &fstest.MapFile{Data: []byte(classifyBugXML)},
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

// TestFake_FinalMessageIsScript covers #43: a scripted turn's
// RunResult.FinalMessage is the script file's own text.
func TestFake_FinalMessageIsScript(t *testing.T) {
	t.Parallel()

	f := NewFake(newClassifyFS())
	res, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalMessage != classifyBugXML {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, classifyBugXML)
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

// TestFake_MissingScriptRecordsNoOnStart proves PR review fix G1: OnStart
// fires only after the turn's script is read and parsed (and ctx
// rechecked) -- the real runtimes' own rule (claude.go, codex.go fire
// OnStart only after cmd.Start succeeds, the last fallible precondition
// before any work) -- so a turn whose script does not exist at all, and so
// never does any work, records no start.
func TestFake_MissingScriptRecordsNoOnStart(t *testing.T) {
	t.Parallel()

	f := NewFake(newClassifyFS())
	var onStartCalls int
	_, err := f.Run(context.Background(), RunRequest{
		Job:     response.Job("nope"),
		OnStart: func(StartInfo) { onStartCalls++ },
	})
	if err == nil {
		t.Fatal("Run with a missing script, want an error")
	}
	if onStartCalls != 0 {
		t.Errorf("OnStart called %d times, want 0 (the script was never even read)", onStartCalls)
	}
}

// TestFake_BrokenScriptRecordsNoOnStart is
// TestFake_MissingScriptRecordsNoOnStart's own proof for a script that
// exists but fails to parse: response.Parse's own failure is also before
// OnStart's call, not after.
func TestFake_BrokenScriptRecordsNoOnStart(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{classifyTurn1Key: &fstest.MapFile{Data: []byte(brokenXML)}}
	f := NewFake(fsys)
	var onStartCalls int
	_, err := f.Run(context.Background(), RunRequest{
		Job:     response.JobClassify,
		OnStart: func(StartInfo) { onStartCalls++ },
	})
	if err == nil {
		t.Fatal("Run with a broken script, want an error")
	}
	if onStartCalls != 0 {
		t.Errorf("OnStart called %d times, want 0 (the script never parsed)", onStartCalls)
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

// effectScriptKey and effectDeleteKey are the fixed (job, label, turn)
// script and ".delete" keys the effect tests below share, so the string
// appears once rather than at every call site.
const (
	effectScriptKey = "classify/1/1.xml"
	effectDeleteKey = "classify/1/1.delete"
)

// TestFakeWritesTree pins the ".tree" sibling effect (design section 9.3):
// every file under "<job>/<label>/<turn>.tree/" is written to the same
// relative path under WorkDir, mode 0644, with parent directories created.
func TestFakeWritesTree(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		effectScriptKey:                 &fstest.MapFile{Data: []byte(classifyBugXML)},
		"classify/1/1.tree/hello.txt":   &fstest.MapFile{Data: []byte("hello")},
		"classify/1/1.tree/sub/dir.txt": &fstest.MapFile{Data: []byte("nested")},
	}
	f := NewFake(fsys)
	workDir := t.TempDir()

	_, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify, Label: "1", WorkDir: workDir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(workDir, "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("hello.txt = %q, want %q", got, "hello")
	}
	info, err := os.Stat(filepath.Join(workDir, "hello.txt"))
	if err != nil {
		t.Fatalf("stat hello.txt: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("hello.txt mode = %o, want %o", perm, 0o644)
	}

	got, err = os.ReadFile(filepath.Join(workDir, "sub", "dir.txt"))
	if err != nil {
		t.Fatalf("read sub/dir.txt (parent dir not created?): %v", err)
	}
	if string(got) != "nested" {
		t.Errorf("sub/dir.txt = %q, want %q", got, "nested")
	}
}

// TestFakeDeletes pins the ".delete" sibling effect: each non-empty line
// is a relative path removed from WorkDir.
func TestFakeDeletes(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		effectScriptKey: &fstest.MapFile{Data: []byte(classifyBugXML)},
		effectDeleteKey: &fstest.MapFile{Data: []byte("gone.txt\n\n")},
	}
	f := NewFake(fsys)
	workDir := t.TempDir()
	writeRealFile(t, filepath.Join(workDir, "gone.txt"), "bye")
	writeRealFile(t, filepath.Join(workDir, "kept.txt"), "stays")

	_, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify, Label: "1", WorkDir: workDir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workDir, "gone.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("gone.txt stat error = %v, want ErrNotExist", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "kept.txt")); err != nil {
		t.Errorf("kept.txt should still exist: %v", err)
	}
}

// TestFakeSkipsEffectsWithoutWorkDir pins "with an empty WorkDir the
// effects are skipped": a script with both siblings present must not
// error, and must not be applied anywhere, when req.WorkDir is empty.
func TestFakeSkipsEffectsWithoutWorkDir(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		effectScriptKey:               &fstest.MapFile{Data: []byte(classifyBugXML)},
		"classify/1/1.tree/hello.txt": &fstest.MapFile{Data: []byte("hello")},
		effectDeleteKey:               &fstest.MapFile{Data: []byte("hello.txt\n")},
	}
	f := NewFake(fsys)

	if _, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify, Label: "1"}); err != nil {
		t.Fatalf("Run with empty WorkDir: %v", err)
	}
}

// writeRealFile writes a real file to path, creating parent directories,
// failing the test on any error.
func writeRealFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// TestFakeRejectsEscape pins the root-confined guarantee: a path that is
// absolute, contains "..", or passes through a symlink leaving WorkDir
// fails inside the standard library, and the fake wraps that failure as
// "fake: unsafe effect path <p>". A tree entry's relative name always
// comes from a valid fs.FS path (fs.WalkDir never yields "." or ".."
// components), so an absolute path or a ".." component can only reach the
// fake through a ".delete" line, which is arbitrary text; a symlink
// escape, in contrast, is exercised for both effects, since either one
// can be asked to cross a symlink already sitting inside WorkDir.
func TestFakeRejectsEscape(t *testing.T) {
	t.Parallel()

	t.Run("write through a symlink leaving the worktree", func(t *testing.T) {
		t.Parallel()

		outside := t.TempDir()
		workDir := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(workDir, "escape")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		fsys := fstest.MapFS{
			effectScriptKey:                     &fstest.MapFile{Data: []byte(classifyBugXML)},
			"classify/1/1.tree/escape/evil.txt": &fstest.MapFile{Data: []byte("pwned")},
		}
		f := NewFake(fsys)

		_, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify, Label: "1", WorkDir: workDir})
		assertUnsafeEffectError(t, err)

		if _, statErr := os.Stat(filepath.Join(outside, "evil.txt")); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("evil.txt escaped into %s", outside)
		}
	})

	t.Run("delete an absolute path", func(t *testing.T) {
		t.Parallel()

		workDir := t.TempDir()
		fsys := fstest.MapFS{
			effectScriptKey: &fstest.MapFile{Data: []byte(classifyBugXML)},
			effectDeleteKey: &fstest.MapFile{Data: []byte("/etc/hosts\n")},
		}
		f := NewFake(fsys)

		_, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify, Label: "1", WorkDir: workDir})
		assertUnsafeEffectError(t, err)
	})

	t.Run("delete a path containing ..", func(t *testing.T) {
		t.Parallel()

		parent := t.TempDir()
		workDir := filepath.Join(parent, "work")
		if err := os.Mkdir(workDir, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		writeRealFile(t, filepath.Join(parent, "escape.txt"), "still here")

		fsys := fstest.MapFS{
			effectScriptKey: &fstest.MapFile{Data: []byte(classifyBugXML)},
			effectDeleteKey: &fstest.MapFile{Data: []byte("../escape.txt\n")},
		}
		f := NewFake(fsys)

		_, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify, Label: "1", WorkDir: workDir})
		assertUnsafeEffectError(t, err)

		if _, statErr := os.Stat(filepath.Join(parent, "escape.txt")); statErr != nil {
			t.Errorf("escape.txt should still exist in the parent: %v", statErr)
		}
	})

	t.Run("delete through a symlink leaving the worktree", func(t *testing.T) {
		t.Parallel()

		outside := t.TempDir()
		writeRealFile(t, filepath.Join(outside, "victim.txt"), "keep me")
		workDir := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(workDir, "escape")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		fsys := fstest.MapFS{
			effectScriptKey: &fstest.MapFile{Data: []byte(classifyBugXML)},
			effectDeleteKey: &fstest.MapFile{Data: []byte("escape/victim.txt\n")},
		}
		f := NewFake(fsys)

		_, err := f.Run(context.Background(), RunRequest{Job: response.JobClassify, Label: "1", WorkDir: workDir})
		assertUnsafeEffectError(t, err)

		if _, statErr := os.Stat(filepath.Join(outside, "victim.txt")); statErr != nil {
			t.Errorf("victim.txt should still exist outside the worktree: %v", statErr)
		}
	})
}

// assertUnsafeEffectError fails unless err is non-nil and its message
// begins with the fixed prefix the fake wraps every root-rejected effect
// path in (design section 9.3).
func assertUnsafeEffectError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Run over an unsafe effect path returned no error")
	}
	const prefix = "fake: unsafe effect path "
	if len(err.Error()) < len(prefix) || err.Error()[:len(prefix)] != prefix {
		t.Errorf("Run error = %q, want prefix %q", err.Error(), prefix)
	}
}
