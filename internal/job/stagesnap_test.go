// stagesnap_test.go builds each pipeline test fixture ("stage": reviewing,
// judging, shipping, published, building) once per test process and gives
// every test its own private copy, instead of replaying the real handlers
// from scratch at every call site (ticket #102, split from #82/#57). A
// stage's builder runs once, under sync.Once; its result is snapshotted to
// a process-lifetime directory (snapshotStage) and copied into the
// caller's own t.TempDir() on every call (copyStage).
//
// This file imports zing/internal/store for *store.Store and store.Open;
// that import's own blank import of modernc.org/sqlite is what registers
// the "sqlite" driver this file's own database/sql calls use.
package job

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zing/internal/gitfixture"
	"zing/internal/store"
)

// TestStageSnapshotCopiesAreIndependent builds one stage from a seeded,
// git-backed, origin-bearing ticket, takes two copies, and checks that
// each copy's store, repo, and origin are its own: a change in one copy
// is invisible in the other.
func TestStageSnapshotCopiesAreIndependent(t *testing.T) {
	s := newPostbuildTestStore(t)
	ticketID := pbSeedQueuedGitBackedTicket(t, s)
	proj, err := s.ProjectForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	originDir, err := gitfixture.WithBareOrigin(t.Context(), proj.LocalPath)
	if err != nil {
		t.Fatalf("WithBareOrigin: %v", err)
	}

	st := &stageSnap{name: "test"}
	build := stageBuild{
		Store: s, DBPath: filepath.Join(s.Dir(), "zing.db"), TicketID: ticketID, OriginDir: originDir,
	}
	if buildErr := snapshotStage(t.Context(), st, build); buildErr != nil {
		t.Fatalf("snapshotStage: %v", buildErr)
	}

	c1 := copyStage(t, st)
	c2 := copyStage(t, st)

	if c1.RepoDir == c2.RepoDir {
		t.Fatalf("copyStage: both copies share RepoDir %s", c1.RepoDir)
	}

	var projectID int64
	for i, c := range []stageCopy{c1, c2} {
		p, projErr := c.Store.ProjectForTicket(t.Context(), c.TicketID)
		if projErr != nil {
			t.Fatalf("copy %d: ProjectForTicket: %v", i+1, projErr)
		}
		if p.LocalPath != c.RepoDir {
			t.Fatalf("copy %d: LocalPath = %s, want %s", i+1, p.LocalPath, c.RepoDir)
		}
		projectID = p.ID

		out, gitErr := gitfixture.Git(t.Context(), c.RepoDir, "remote", "get-url", "origin")
		if gitErr != nil {
			t.Fatalf("copy %d: git remote get-url: %v: %s", i+1, gitErr, out)
		}
		if got := strings.TrimSpace(string(out)); got != c.OriginDir {
			t.Fatalf("copy %d: origin url = %s, want %s", i+1, got, c.OriginDir)
		}

		ticket, ticketErr := c.Store.GetTicket(t.Context(), c.TicketID)
		if ticketErr != nil {
			t.Fatalf("copy %d: GetTicket: %v", i+1, ticketErr)
		}
		if ticket.State != stateQueued {
			t.Fatalf("copy %d: ticket state = %s, want %s", i+1, ticket.State, stateQueued)
		}
	}

	newID, err := c1.Store.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "fake#2", Title: pbTicketTitle, State: stateQueued,
	})
	if err != nil {
		t.Fatalf("copy 1: InsertTicket: %v", err)
	}
	if _, err := c2.Store.GetTicket(t.Context(), newID); err == nil {
		t.Fatalf("copy 2: GetTicket found copy 1's inserted ticket %d", newID)
	}
}

// stageSnap is one pipeline stage's process-lifetime snapshot. It is
// always a package-level pointer; its fields are read and written only by
// useStage, snapshotStage, and copyStage. useStage (task 5) adds the
// sync.Once and the failedBy bookkeeping that make the build run at most
// once per process; snapshotStage and copyStage need neither.
type stageSnap struct {
	name string

	dir       string // SNAP, from os.MkdirTemp; empty until the build succeeds
	ticketID  int64
	repoPath  string // projects.local_path as stored in SNAP/zing.db
	hasOrigin bool
	extra     any
}

// stageBuild is what a stage's builder hands snapshotStage: an open store,
// its file path, and the ticket whose project is the repo to snapshot.
type stageBuild struct {
	Store     *store.Store
	DBPath    string
	TicketID  int64
	OriginDir string // a bare repo to snapshot too, or empty
	Extra     any
}

// stageCopy is one test's own private copy of a stage, built by copyStage
// inside the caller's t.TempDir().
type stageCopy struct {
	Store     *store.Store
	DBPath    string
	TicketID  int64
	RepoDir   string
	OriginDir string // set only when the stage has one
	Extra     any
}

// snapshotStage builds st's process-lifetime snapshot from b: a VACUUM
// INTO copy of b.DBPath, a gitfixture.CopyRepo copy of b.TicketID's
// project repo, and, when b.OriginDir is set, a copy of that bare origin
// too. It is called at most once per stage, inside the first useStage
// caller's sync.Once.Do.
func snapshotStage(ctx context.Context, st *stageSnap, b stageBuild) error {
	if b.Store == nil || b.DBPath == "" || b.TicketID == 0 {
		return fmt.Errorf("stage %s: builder returned an incomplete stageBuild", st.name)
	}

	dir, err := os.MkdirTemp("", "zing-stage-"+st.name+"-")
	if err != nil {
		return fmt.Errorf("stage %s: snapshot dir: %w", st.name, err)
	}

	proj, err := b.Store.ProjectForTicket(ctx, b.TicketID)
	if err != nil {
		return fmt.Errorf("stage %s: project for ticket: %w", st.name, err)
	}

	// A second database/sql connection, not b.Store's own: the store runs
	// in WAL mode (internal/store/store.go), so a plain file copy can miss
	// rows still sitting in the -wal file. VACUUM INTO writes one
	// consistent file.
	vdb, err := sql.Open("sqlite", b.DBPath)
	if err != nil {
		return fmt.Errorf("stage %s: open db for vacuum: %w", st.name, err)
	}
	defer vdb.Close()
	snapDB := filepath.Join(dir, "zing.db")
	if _, err := vdb.ExecContext(ctx, "VACUUM INTO ?", snapDB); err != nil {
		return fmt.Errorf("stage %s: vacuum into: %w", st.name, err)
	}

	if err := gitfixture.CopyRepo(ctx, proj.LocalPath, filepath.Join(dir, "repo")); err != nil {
		return fmt.Errorf("stage %s: snapshot repo: %w", st.name, err)
	}
	if b.OriginDir != "" {
		if err := gitfixture.CopyRepo(ctx, b.OriginDir, filepath.Join(dir, "origin")); err != nil {
			return fmt.Errorf("stage %s: snapshot origin: %w", st.name, err)
		}
	}

	st.dir = dir
	st.ticketID = b.TicketID
	st.repoPath = proj.LocalPath
	st.hasOrigin = b.OriginDir != ""
	st.extra = b.Extra
	return nil
}

// copyStage copies st's process-lifetime snapshot into the caller's own
// t.TempDir(): the db file, the repo, and, when the stage has one, the
// origin, then rewrites the copy's projects.local_path to match the
// copied repo before opening it as a *store.Store.
func copyStage(t *testing.T, st *stageSnap) stageCopy {
	t.Helper()
	dir := t.TempDir()

	dbBytes, err := os.ReadFile(filepath.Join(st.dir, "zing.db")) //nolint:gosec // G304: st.dir is this package's own process-lifetime snapshot dir
	if err != nil {
		t.Fatalf("stage %s: copy: read snapshot db: %v", st.name, err)
	}
	dbPath := filepath.Join(dir, "zing.db")
	if writeErr := os.WriteFile(dbPath, dbBytes, 0o600); writeErr != nil {
		t.Fatalf("stage %s: copy: write db copy: %v", st.name, writeErr)
	}

	repoDir := filepath.Join(dir, "repo")
	if copyErr := gitfixture.CopyRepo(t.Context(), filepath.Join(st.dir, "repo"), repoDir); copyErr != nil {
		t.Fatalf("stage %s: copy: copy repo: %v", st.name, copyErr)
	}

	var originDir string
	if st.hasOrigin {
		originDir = filepath.Join(dir, "origin")
		if copyErr := gitfixture.CopyRepo(t.Context(), filepath.Join(st.dir, "origin"), originDir); copyErr != nil {
			t.Fatalf("stage %s: copy: copy origin: %v", st.name, copyErr)
		}
		if out, remoteErr := gitfixture.Git(t.Context(), repoDir, "remote", "set-url", "origin", originDir); remoteErr != nil {
			t.Fatalf("stage %s: copy: remote set-url: %v: %s", st.name, remoteErr, out)
		}
	}

	if rewriteErr := rewriteLocalPath(t.Context(), dbPath, st.repoPath, repoDir); rewriteErr != nil {
		t.Fatalf("stage %s: copy: %v", st.name, rewriteErr)
	}

	s, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("stage %s: copy: store.Open: %v", st.name, err)
	}
	t.Cleanup(func() { _ = s.Close() })

	return stageCopy{
		Store: s, DBPath: dbPath, TicketID: st.ticketID,
		RepoDir: repoDir, OriginDir: originDir, Extra: st.extra,
	}
}

// rewriteLocalPath points the one project row whose local_path is oldPath
// at newPath, over a direct database/sql connection opened and closed
// around the single UPDATE: internal/store has no update method for
// local_path, and this ticket must not change non-test code. The UPDATE
// must affect exactly one row, since a copied snapshot's db always has
// exactly one project at oldPath (the stage's own project).
func rewriteLocalPath(ctx context.Context, dbPath, oldPath, newPath string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("open db for local_path update: %w", err)
	}
	defer db.Close()

	res, err := db.ExecContext(ctx, "UPDATE projects SET local_path = ? WHERE local_path = ?", newPath, oldPath)
	if err != nil {
		return fmt.Errorf("update local_path: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update local_path: rows affected: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("update local_path: affected %d rows, want 1", rows)
	}
	return nil
}
