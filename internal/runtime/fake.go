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

// Run implements Runtime: it honors ctx cancellation, resolves req's session
// (minting a new one, or resuming an existing one), reads and parses that
// session's next turn's script, and, only once that succeeds, advances the
// session's turn and commits a newly minted session (design section 6.9).
func (f *Fake) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	sessionID, sess, isNew, err := f.resolveSessionLocked(req)
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
	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}

	sess.nextTurn++
	if isNew {
		// Commit a new session only after its first turn read and parsed, so
		// a failed first turn leaves no unreachable session in the map.
		f.sessions[sessionID] = sess
	}
	return RunResult{Response: doc.Response, SessionID: sessionID}, nil
}

// resolveSessionLocked mints a new session for an empty req.SessionID (the
// caller commits it to the map only after a successful first turn), or looks
// up and validates an existing one for a resume. The bool result reports
// whether the session is newly minted. Callers must hold f.mu.
func (f *Fake) resolveSessionLocked(req RunRequest) (id string, sess *fakeSession, isNew bool, err error) {
	if req.SessionID == "" {
		id = fmt.Sprintf("fake-%d", fakeSessionCounter.Add(1))
		sess = &fakeSession{job: req.Job, label: req.Label, nextTurn: 1}
		return id, sess, true, nil
	}

	found, ok := f.sessions[req.SessionID]
	if !ok {
		return "", nil, false, fmt.Errorf("fake: unknown session %s", req.SessionID)
	}
	if found.job != req.Job || found.label != req.Label {
		return "", nil, false, fmt.Errorf(
			"fake: session %s is (%s, %s), resume asked for (%s, %s)",
			req.SessionID, found.job, found.label, req.Job, req.Label,
		)
	}
	return req.SessionID, found, false, nil
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
