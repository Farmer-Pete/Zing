package runtime

import (
	"context"
	"fmt"
	"io/fs"
	"sync"
	"sync/atomic"

	"zing/internal/response"
)

var _ Runtime = (*Fake)(nil)

// fakeSessionCounter mints globally unique session ids across every Fake
// instance in the process (design section 6.9), so two NewFake values
// never mint the same id.
var fakeSessionCounter atomic.Uint64

// fakeSession is one session's recorded (job, label) pair and the next
// turn it will serve.
type fakeSession struct {
	job      response.Job
	label    string
	nextTurn int
}

// Fake is a scripted Runtime that reads a canned <zing> document for each
// turn from an fs.FS keyed "<job>/<label>/<turn>.xml" (empty label
// collapses to "<job>/<turn>.xml"), design section 7.4. It never calls a
// real model, which is what makes it useful in a test.
type Fake struct {
	fsys fs.FS

	mu       sync.Mutex
	sessions map[string]*fakeSession
}

// NewFake returns a Fake that serves its documents from fsys.
func NewFake(fsys fs.FS) *Fake {
	return &Fake{fsys: fsys, sessions: make(map[string]*fakeSession)}
}

// Run implements Runtime: it resolves req's session (minting a new one, or
// resuming an existing one), reads and parses that session's next turn's
// script, and, only once that succeeds, advances the session's turn
// (design section 6.9).
func (f *Fake) Run(_ context.Context, req RunRequest) (RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	sessionID, sess, err := f.resolveSessionLocked(req)
	if err != nil {
		return RunResult{}, err
	}

	key := scriptKey(req.Job, req.Label, sess.nextTurn)
	data, err := fs.ReadFile(f.fsys, key)
	if err != nil {
		return RunResult{}, fmt.Errorf("fake: no script for %s", key)
	}
	doc, err := response.Parse(data)
	if err != nil {
		return RunResult{}, err
	}

	sess.nextTurn++
	return RunResult{Response: doc.Response, SessionID: sessionID}, nil
}

// resolveSessionLocked mints a new session for an empty req.SessionID, or
// looks up and validates an existing one for a resume. Callers must hold
// f.mu.
func (f *Fake) resolveSessionLocked(req RunRequest) (string, *fakeSession, error) {
	if req.SessionID == "" {
		id := fmt.Sprintf("fake-%d", fakeSessionCounter.Add(1))
		sess := &fakeSession{job: req.Job, label: req.Label, nextTurn: 1}
		f.sessions[id] = sess
		return id, sess, nil
	}

	sess, ok := f.sessions[req.SessionID]
	if !ok {
		return "", nil, fmt.Errorf("fake: unknown session %s", req.SessionID)
	}
	if sess.job != req.Job || sess.label != req.Label {
		return "", nil, fmt.Errorf(
			"fake: session %s is (%s, %s), resume asked for (%s, %s)",
			req.SessionID, sess.job, sess.label, req.Job, req.Label,
		)
	}
	return req.SessionID, sess, nil
}

// scriptKey builds the fake-runtime script key for turn of (job, label):
// "<job>/<label>/<turn>.xml", or "<job>/<turn>.xml" when label is empty
// (design section 7.4).
func scriptKey(job response.Job, label string, turn int) string {
	if label == "" {
		return fmt.Sprintf("%s/%d.xml", job, turn)
	}
	return fmt.Sprintf("%s/%s/%d.xml", job, label, turn)
}
