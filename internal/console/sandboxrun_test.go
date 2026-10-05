package console_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// sandboxRunResponseBody mirrors sandboxrun.go's own unexported
// sandboxRunResponse: this package cannot reach that type, so the test
// decodes into its own copy of the same wire shape.
type sandboxRunResponseBody struct {
	Exit     int    `json:"exit"`
	TimedOut bool   `json:"timed_out"`
	Output   string `json:"output"`
	Total    int64  `json:"total"`
	Cut      bool   `json:"cut"`
}

// newSandboxRunFixture builds a store, a project on a real, signed git
// repository with a bare origin (gitfixture.NewSigningRepo,
// gitfixture.WithBareOrigin), one queued ticket, and an orchestrator wired
// to this package's own working GitHub stub (resumeE2EShipGitHub). When
// withWorktree is true, the ticket's worktree is created up front
// (orch.EnsureWorktree) -- TicketCommands.Run itself never creates one. It
// starts console.New on a reserved 127.0.0.1 listener (mirroring
// mw_test.go's newMutationTestServer), with a job.TicketCommands backed by
// cmds as the route's TicketRunner.
func newSandboxRunFixture(t *testing.T, cmds job.CommandRunner, withWorktree bool) (srv *httptest.Server, handler http.Handler, port int, ticketID int64, repoDir string) {
	t.Helper()

	s := newConsoleTestStore(t)
	repoDir = t.TempDir()
	if err := gitfixture.NewSigningRepo(t.Context(), repoDir); err != nil {
		t.Fatalf("gitfixture.NewSigningRepo: %v", err)
	}
	if _, err := gitfixture.WithBareOrigin(t.Context(), repoDir); err != nil {
		t.Fatalf("gitfixture.WithBareOrigin: %v", err)
	}

	projectID, err := s.EnsureProject(t.Context(), store.Project{
		Name: "sandbox-run", RepoURL: "https://example.invalid/sandbox-run", LocalPath: repoDir, Tracker: testTrackerGitHub,
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err = s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "fake#1", Title: "sandbox run fixture", State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	orch, err := orchestrator.New(
		orchestrator.Project{Owner: "fixture", Repo: "fixture", LocalPath: repoDir, DefaultBranch: "main"},
		&resumeE2EShipGitHub{}, orchestrator.NewRunner(), nil)
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	repoGit, err := orch.GitCommonDir(t.Context())
	if err != nil {
		t.Fatalf("GitCommonDir: %v", err)
	}
	if withWorktree {
		if _, _, wtErr := orch.EnsureWorktree(t.Context(), ticketID, "sandbox run"); wtErr != nil {
			t.Fatalf("EnsureWorktree: %v", wtErr)
		}
	}

	tc := job.TicketCommands{
		Store:   s,
		Machine: testMachine(t),
		Projects: map[int64]job.Project{
			projectID: {Orch: orch, RepoGit: repoGit},
		},
		Commands: cmds,
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a listener: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}
	port = addr.Port

	handler = console.New(s, bus.New(), nil, []string{testBindHost}, port, newTestLogHandler(t), nil, testPushToken, response.SeverityMinor, "", nil, "", tc)
	srv = httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the placeholder listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, handler, port, ticketID, repoDir
}

// sandboxRunWorktreeDir is ticketID's worktree directory under repoDir,
// resolved: pwd (through /bin/sh) reports the kernel's own physical cwd,
// with every symlink -- including macOS's default TMPDIR -- resolved.
func sandboxRunWorktreeDir(t *testing.T, repoDir string, ticketID int64) string {
	t.Helper()
	dir := filepath.Join(repoDir, ".zing", "wt", strconv.FormatInt(ticketID, 10))
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	return resolved
}

// sandboxRunRequest builds a same-origin POST /tickets/{id}/sandbox-run
// request against srv, the way mw_test.go's mutationRequest builds its own
// same-origin requests.
func sandboxRunRequest(t *testing.T, srv *httptest.Server, port, ticketID int64, cmd string) *http.Request {
	t.Helper()
	body, err := json.Marshal(sandboxRunReqBody{Cmd: cmd})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return sandboxRunRawRequest(t, srv, port, ticketID, body)
}

// sandboxRunReqBody is this test's own copy of sandboxrun.go's unexported
// sandboxRunRequest wire shape.
type sandboxRunReqBody struct {
	Cmd string `json:"cmd"`
}

// sandboxRunRawRequest is sandboxRunRequest with a caller-built body, for
// the malformed-JSON and unknown-field cases TestSandboxRunBadRequest
// drives.
func sandboxRunRawRequest(t *testing.T, srv *httptest.Server, port, ticketID int64, body []byte) *http.Request {
	t.Helper()
	return sandboxRunRawRequestPath(t, srv, port, strconv.FormatInt(ticketID, 10), body)
}

// sandboxRunRawRequestPath is sandboxRunRawRequest with the {id} path
// segment given literally, for TestSandboxRunBadRequest's non-numeric id
// case.
func sandboxRunRawRequestPath(t *testing.T, srv *httptest.Server, port int64, idPath string, body []byte) *http.Request {
	t.Helper()
	authority := "127.0.0.1:" + strconv.FormatInt(port, 10)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/tickets/"+idPath+"/sandbox-run", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = authority
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Origin", "http://"+authority)
	return req
}

// decodeSandboxRunResponse json-decodes resp.Body into a
// sandboxRunResponseBody, failing the test on error. The caller owns
// closing resp.Body (bodyclose wants the close visible at the call site,
// not hidden behind a helper).
func decodeSandboxRunResponse(t *testing.T, resp *http.Response) sandboxRunResponseBody {
	t.Helper()
	var body sandboxRunResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

// sandboxRunAssertExit posts cmd and asserts the response's exit code and
// timed_out, used by TestSandboxRunExitCodes for both of its subtests.
func sandboxRunAssertExit(t *testing.T, srv *httptest.Server, port, ticketID int64, cmd string, wantExit int) {
	t.Helper()
	resp := doRequest(t, sandboxRunRequest(t, srv, port, ticketID, cmd))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != contentTypeJSONForTest {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSONForTest)
	}
	body := decodeSandboxRunResponse(t, resp)
	if body.Exit != wantExit {
		t.Errorf("exit = %d, want %d", body.Exit, wantExit)
	}
	if body.TimedOut {
		t.Error("timed_out is true, want false")
	}
}

// TestSandboxRunExitCodes proves a command that exits cleanly and one that
// exits with status 1 run through the endpoint report exit 0 and exit 1
// respectively, with timed_out false.
func TestSandboxRunExitCodes(t *testing.T) {
	t.Parallel()
	cmds := job.NewCommandRunner(sandbox.Off(), false)
	srv, _, port, ticketID, _ := newSandboxRunFixture(t, cmds, true)

	t.Run("exits 0", func(t *testing.T) {
		t.Parallel()
		sandboxRunAssertExit(t, srv, int64(port), ticketID, "true", 0)
	})
	t.Run("exits 1", func(t *testing.T) {
		t.Parallel()
		sandboxRunAssertExit(t, srv, int64(port), ticketID, "false", 1)
	})
}

// contentTypeJSONForTest mirrors the console package's own unexported
// contentTypeJSON constant.
const contentTypeJSONForTest = "application/json"

// TestSandboxRunOutput proves stdout and stderr interleave in the response,
// in the order the command wrote them, and that cut is false under the cap.
func TestSandboxRunOutput(t *testing.T) {
	t.Parallel()
	cmds := job.NewCommandRunner(sandbox.Off(), false)
	srv, _, port, ticketID, repoDir := newSandboxRunFixture(t, cmds, true)
	wantDir := sandboxRunWorktreeDir(t, repoDir, ticketID)

	resp := doRequest(t, sandboxRunRequest(t, srv, int64(port), ticketID, "pwd; echo to-stderr >&2"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeSandboxRunResponse(t, resp)
	wantPrefix := wantDir + "\n"
	if !strings.HasPrefix(body.Output, wantPrefix) {
		t.Errorf("output = %q, want it to start with %q", body.Output, wantPrefix)
	}
	if !strings.Contains(body.Output, "to-stderr") {
		t.Errorf("output = %q, want it to contain %q", body.Output, "to-stderr")
	}
	if body.Cut {
		t.Errorf("cut = true, want false")
	}
}

// TestSandboxRunOutputCapped proves the response keeps only the last 16384
// bytes of a larger output, and reports the full byte count and cut=true.
func TestSandboxRunOutputCapped(t *testing.T) {
	t.Parallel()
	cmds := job.NewCommandRunner(sandbox.Off(), false)
	srv, _, port, ticketID, _ := newSandboxRunFixture(t, cmds, true)

	resp := doRequest(t, sandboxRunRequest(t, srv, int64(port), ticketID, "head -c 20000 /dev/zero | tr '\\0' a"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeSandboxRunResponse(t, resp)
	if body.Total != 20000 {
		t.Errorf("total = %d, want 20000", body.Total)
	}
	if !body.Cut {
		t.Errorf("cut = false, want true")
	}
	if len(body.Output) != 16384 {
		t.Errorf("len(output) = %d, want 16384", len(body.Output))
	}
}

// TestSandboxRunNoWorktree proves a ticket with no worktree refuses with
// 409 and that nothing is created on disk.
func TestSandboxRunNoWorktree(t *testing.T) {
	t.Parallel()
	cmds := job.NewCommandRunner(sandbox.Off(), false)
	srv, _, port, ticketID, repoDir := newSandboxRunFixture(t, cmds, false)

	resp := doRequest(t, sandboxRunRequest(t, srv, int64(port), ticketID, "true"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	wtDir := filepath.Join(repoDir, ".zing", "wt", strconv.FormatInt(ticketID, 10))
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want it not to exist", wtDir, err)
	}
}

// TestSandboxRunUnknownTicket proves an id naming no ticket at all gets 404
// "ticket not found".
func TestSandboxRunUnknownTicket(t *testing.T) {
	t.Parallel()
	cmds := job.NewCommandRunner(sandbox.Off(), false)
	srv, _, port, _, _ := newSandboxRunFixture(t, cmds, true)

	resp := doRequest(t, sandboxRunRequest(t, srv, int64(port), 999, "true"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// countingCommands wraps a real job.CommandRunner, counting every call, so
// TestSandboxRunBadRequest can prove each of its four refusals never ran
// anything.
type countingCommands struct {
	real  job.CommandRunner
	calls int
}

func (c *countingCommands) Run(ctx context.Context, dir, repoGit, shellCmd string, timeout time.Duration, cio job.CommandIO) (int, error) {
	c.calls++
	return c.real.Run(ctx, dir, repoGit, shellCmd, timeout, cio)
}

// TestSandboxRunBadRequest drives the four 400 shapes -- whitespace-only
// cmd, a 4097-byte cmd, an unknown JSON key, and a non-numeric id -- and
// proves none of them ever reached the runner.
func TestSandboxRunBadRequest(t *testing.T) {
	t.Parallel()
	counting := &countingCommands{real: job.NewCommandRunner(sandbox.Off(), false)}
	srv, _, port, ticketID, _ := newSandboxRunFixture(t, counting, true)
	// Registered on the parent t, so it runs once every subtest below --
	// parallel, so they do not finish until this function returns -- has
	// actually completed; checking counting.calls right after the loop
	// would race against subtests that have only been scheduled, not run.
	t.Cleanup(func() {
		if counting.calls != 0 {
			t.Errorf("runner calls = %d, want 0", counting.calls)
		}
	})

	tests := []struct {
		name string
		req  func() *http.Request
	}{
		{"whitespace-only cmd", func() *http.Request {
			return sandboxRunRequest(t, srv, int64(port), ticketID, "   ")
		}},
		{"4097-byte cmd", func() *http.Request {
			return sandboxRunRequest(t, srv, int64(port), ticketID, strings.Repeat("a", 4097))
		}},
		{"unknown JSON key", func() *http.Request {
			return sandboxRunRawRequest(t, srv, int64(port), ticketID, []byte(`{"cmd":"true","oops":1}`))
		}},
		{"non-numeric id", func() *http.Request {
			return sandboxRunRawRequestPath(t, srv, int64(port), "abc", []byte(`{"cmd":"true"}`))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := doRequest(t, tc.req())
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

// TestSandboxRunRefusesNonLoopback proves a request with a valid Host and
// Origin but a non-loopback RemoteAddr is still refused, driven straight
// against the handler (no real listener) so RemoteAddr can be set directly.
func TestSandboxRunRefusesNonLoopback(t *testing.T) {
	t.Parallel()
	cmds := job.NewCommandRunner(sandbox.Off(), false)
	_, handler, port, ticketID, _ := newSandboxRunFixture(t, cmds, true)

	authority := "127.0.0.1:" + strconv.Itoa(port)
	body := []byte(`{"cmd":"true"}`)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+authority+"/tickets/"+strconv.FormatInt(ticketID, 10)+"/sandbox-run", bytes.NewReader(body))
	req.Host = authority
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Origin", "http://"+authority)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// TestSandboxRunCrossOrigin proves a cross-site Origin is refused by the
// same mutation guard every other POST route sits behind.
func TestSandboxRunCrossOrigin(t *testing.T) {
	t.Parallel()
	cmds := job.NewCommandRunner(sandbox.Off(), false)
	srv, _, port, ticketID, _ := newSandboxRunFixture(t, cmds, true)

	req := sandboxRunRequest(t, srv, int64(port), ticketID, "true")
	req.Header.Set("Origin", "http://evil.example")
	resp := doRequest(t, req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestSandboxRunSandboxUnavailable proves an unavailable, required sandbox
// reports 503 "build sandbox unavailable", the same refusal CHECK itself
// would hit.
func TestSandboxRunSandboxUnavailable(t *testing.T) {
	t.Parallel()
	cmds := job.NewCommandRunner(sandbox.NotLoaded(), true)
	srv, _, port, ticketID, _ := newSandboxRunFixture(t, cmds, true)

	resp := doRequest(t, sandboxRunRequest(t, srv, int64(port), ticketID, "true"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestSandboxRunNotConfigured proves a console built with a nil TicketRunner
// answers the route with 503 "sandbox runs are not available".
func TestSandboxRunNotConfigured(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "no runner configured")

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a listener: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}
	handler := console.New(s, bus.New(), nil, []string{testBindHost}, addr.Port, newTestLogHandler(t), nil, testPushToken, response.SeverityMinor, "", nil, "", nil)
	srv := httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the placeholder listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	resp := doRequest(t, sandboxRunRequest(t, srv, int64(addr.Port), ticketID, "true"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
