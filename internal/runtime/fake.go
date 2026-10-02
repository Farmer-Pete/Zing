package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
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

	// Re-check after the lock: ctx may have been cancelled while Run waited
	// on f.mu, and a cancelled request must consume no scripted turn.
	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}

	sessionID, sess, isNew, err := f.resolveSessionLocked(req)
	if err != nil {
		return RunResult{}, err
	}

	// The start handshake's fake-runtime twin (design section 7.1, #45):
	// PID 0 since there is no real process, called while f.mu is held, so
	// OnStart must never call back into this Fake.
	if req.OnStart != nil {
		req.OnStart(StartInfo{PID: 0, SessionID: sessionID})
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

	if err := f.applyEffects(req.Job, req.Label, sess.nextTurn, req.WorkDir); err != nil {
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
	return effectKey(job, label, turn, ".xml")
}

// effectKey builds the fake-runtime key for one of a turn's files: the
// script itself (suffix ".xml") or one of the two optional siblings
// (".tree", ".delete"), all sharing the same "<job>/<label>/<turn>" stem,
// or "<job>/<turn>" when label is empty (design section 7.4, section
// 9.3).
func effectKey(job response.Job, label string, turn int, suffix string) string {
	if label == "" {
		return fmt.Sprintf("%s/%d%s", job, turn, suffix)
	}
	return fmt.Sprintf("%s/%s/%d%s", job, label, turn, suffix)
}

// applyEffects writes one turn's optional file effects into workDir
// (design section 9.3): every file under the "<job>/<label>/<turn>.tree/"
// sibling directory, then every path named in the
// "<job>/<label>/<turn>.delete" sibling file. An empty workDir skips
// both, since a run with nothing to diff has nowhere safe to write.
// Neither sibling need exist; when neither does, workDir is never even
// opened, so a script with no file effects places no requirement on
// workDir actually existing on disk.
func (f *Fake) applyEffects(job response.Job, label string, turn int, workDir string) error {
	if workDir == "" {
		return nil
	}

	treeDir := effectKey(job, label, turn, ".tree")
	deleteFile := effectKey(job, label, turn, ".delete")

	hasTree, err := existsInFS(f.fsys, treeDir)
	if err != nil {
		return fmt.Errorf("fake: stat %s: %w", treeDir, err)
	}
	hasDelete, err := existsInFS(f.fsys, deleteFile)
	if err != nil {
		return fmt.Errorf("fake: stat %s: %w", deleteFile, err)
	}
	if !hasTree && !hasDelete {
		return nil
	}

	root, err := os.OpenRoot(workDir)
	if err != nil {
		return fmt.Errorf("fake: open work dir %s: %w", workDir, err)
	}
	defer root.Close()

	if hasTree {
		if err := f.writeTree(root, treeDir); err != nil {
			return err
		}
	}
	if hasDelete {
		return f.applyDeletes(root, deleteFile)
	}
	return nil
}

// existsInFS reports whether name exists in fsys, treating fs.ErrNotExist
// as "no" rather than an error: an optional sibling that is simply absent
// is not a failure.
func existsInFS(fsys fs.FS, name string) (bool, error) {
	if _, err := fs.Stat(fsys, name); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// writeTree copies every regular file under fsys's treeDir to the same
// relative path under root, mode 0644, creating parent directories as it
// goes. treeDir is assumed to exist; applyEffects checks that first.
// Every write goes through root, so os.OpenRoot's own confinement — no
// absolute path, no "..", no symlink leaving the directory root opened —
// applies to every destination path; a rejected path surfaces as "fake:
// unsafe effect path <p>" (design section 9.3).
func (f *Fake) writeTree(root *os.Root, treeDir string) error {
	return fs.WalkDir(f.fsys, treeDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel := strings.TrimPrefix(p, treeDir+"/")
		data, readErr := fs.ReadFile(f.fsys, p)
		if readErr != nil {
			return fmt.Errorf("fake: read %s: %w", p, readErr)
		}

		if dir := path.Dir(rel); dir != "." {
			if mkErr := root.MkdirAll(dir, 0o755); mkErr != nil {
				return fmt.Errorf("fake: unsafe effect path %s: %w", rel, mkErr)
			}
		}
		if writeErr := root.WriteFile(rel, data, 0o644); writeErr != nil {
			return fmt.Errorf("fake: unsafe effect path %s: %w", rel, writeErr)
		}
		return nil
	})
}

// applyDeletes removes, through root, every non-empty line of fsys's
// deleteFile as a relative path. deleteFile is assumed to exist;
// applyEffects checks that first. Every removal goes through root, so the
// same confinement writeTree relies on applies here too (design section
// 9.3).
func (f *Fake) applyDeletes(root *os.Root, deleteFile string) error {
	data, err := fs.ReadFile(f.fsys, deleteFile)
	if err != nil {
		return fmt.Errorf("fake: read %s: %w", deleteFile, err)
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if err := root.Remove(line); err != nil {
			return fmt.Errorf("fake: unsafe effect path %s: %w", line, err)
		}
	}
	return nil
}
