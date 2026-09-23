package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"time"

	zing "zing"
	"zing/fixtures"
	"zing/internal/bus"
	"zing/internal/console"
	zdispatch "zing/internal/dispatch"
	"zing/internal/job"
	"zing/internal/lens"
	"zing/internal/machine"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/schemagen"
	"zing/internal/store"
	"zing/internal/tracker"
)

// runSelftest proves the foundation on an empty machine: it migrates a fresh
// temporary database and checks it. It prints "selftest: OK" and returns 0
// when every step passes, or prints "selftest: <detail>" for the first
// failure and returns 1.
func runSelftest() int {
	if err := selftest(); err != nil {
		fmt.Fprintf(os.Stderr, "selftest: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, "selftest: OK")
	return 0
}

func selftest() error {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "zing-selftest")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	s, err := store.Open(ctx, filepath.Join(dir, "zing.db"))
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	if err = s.VerifyTables(ctx); err != nil {
		return fmt.Errorf("verify tables: %w", err)
	}

	diffs, err := schemagen.Diff(store.SchemaFS())
	if err != nil {
		return fmt.Errorf("schema drift: %w", err)
	}
	if len(diffs) > 0 {
		return fmt.Errorf("committed schema differs from the generator: %s", diffs[0])
	}

	if _, err = machine.Load(zing.Assets, "machine.toml"); err != nil {
		return err
	}

	lenses, err := lens.Load(zing.Assets, "prompts/lenses")
	if err != nil {
		return err
	}
	if len(lenses) != 8 {
		return fmt.Errorf("expected 8 lenses, found %d", len(lenses))
	}

	if err := s.ValidateExamples(); err != nil {
		return err
	}

	if err := checkResponseTemplates(); err != nil {
		return err
	}

	if err := checkResponseExamples(response.ExampleFS); err != nil {
		return err
	}

	if err := selftestE2E(ctx); err != nil {
		return fmt.Errorf("end-to-end: %w", err)
	}

	return nil
}

// e2eMaxTicks bounds selftestResumeE2E's tick loop: enough ticks for intake
// plus one handler call per pipeline transition (design section 7.1:
// queued, planning x2, building, reviewing, judging, shipping is 7 handler
// calls), with generous headroom, so a stuck dispatcher fails the selftest
// promptly instead of hanging.
const e2eMaxTicks = 50

// e2eOwner is this selftest run's claim owner id (design section 7.2's
// shape is <hostname>-<pid>; a fixed literal is simpler and just as unique
// within one selftest process, which claims nothing concurrently).
const e2eOwner = "selftest-e2e"

// e2eWantStates is the ordered "to" state of every state message the
// silent ring plus the one question write, in order (design section 7.1):
// queued -> planning carries no message at intake, so the first message is
// planning itself.
var e2eWantStates = []string{"planning", "building", "reviewing", "judging", "shipping", "done"}

// e2eFrameTimeout bounds every SSE read and every settle-poll this suite
// makes, matching internal/console's own test frameTimeout: long enough for
// a slow CI box, short enough that a hung stream fails selftest instead of
// hanging it.
const e2eFrameTimeout = 5 * time.Second

// e2ePushToken is never checked: neither phase below calls /push/key or
// /push/subscribe, so any value satisfies console.New's signature.
const e2ePushToken = "selftest-e2e-push-token" //nolint:gosec // not a credential: a fixed placeholder no route in this suite ever checks

// selftestE2E proves the design section 11 end-to-end suite plus its Task
// 12 extension (design section 6.15, section 12 row 12), entirely on
// temp-dir stores (never ~/.zing), as the one suite that must never be
// skipped: selftestSeedDemo seeds the demo fixture and proves every
// closed-set question kind renders in the console's real live thread, and
// selftestResumeE2E drives the fixture ticket from intake through planning
// with the real dispatcher, the fixture tracker, the fake runtime, the real
// store, the real bus, and the real console endpoints -- including its own
// live GET /stream -- answers the planning question through POST /draft
// then POST /send exactly as a browser's chip click and send chord would,
// and asserts the resume advances the ticket to done with every state
// message in order and no goroutine or bus-subscription leak.
func selftestE2E(ctx context.Context) error {
	if err := selftestSeedDemo(ctx); err != nil {
		return fmt.Errorf("seed demo: %w", err)
	}
	return selftestResumeE2E(ctx)
}

// selftestSeedDemo seeds the design section 6.15 demo fixture on its own
// temp-dir store, calling SeedDemo twice to also prove it is idempotent,
// then opens the console's real live GET /stream on the demo ticket's
// thread -- the same path a browser's thread view reads -- and asserts all
// six closed-set question kinds (design section 8) render there.
func selftestSeedDemo(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "zing-selftest-seed-demo")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	st, err := store.Open(ctx, filepath.Join(dir, "zing.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if seedErr := console.SeedDemo(ctx, st); seedErr != nil {
		return fmt.Errorf("first SeedDemo: %w", seedErr)
	}
	if seedErr := console.SeedDemo(ctx, st); seedErr != nil {
		return fmt.Errorf("second SeedDemo (idempotency): %w", seedErr)
	}

	ticketID, err := demoTicketID(ctx, st)
	if err != nil {
		return err
	}

	logHandler := console.NewHandler(io.Discard, new(slog.LevelVar))
	srv, err := newSelftestConsoleServer(ctx, st, bus.New(), nil, logHandler)
	if err != nil {
		return fmt.Errorf("start console server: %w", err)
	}
	defer srv.Close()

	streamResp, streamReader, cancelStream, err := getThreadStream(ctx, srv.URL, ticketID)
	if err != nil {
		return fmt.Errorf("open demo thread stream: %w", err)
	}
	defer cancelStream()
	defer func() { _ = streamResp.Body.Close() }()

	_, main, _, err := readInitialSelftestFrames(streamReader)
	if err != nil {
		return fmt.Errorf("read demo thread frames: %w", err)
	}

	return assertEveryQuestionKindRenders(main)
}

// demoTicketID returns SeedDemo's demo ticket id: the one ticket under the
// one project SeedDemo names "demo".
func demoTicketID(ctx context.Context, st *store.Store) (int64, error) {
	projects, err := st.ListProjects(ctx)
	if err != nil {
		return 0, fmt.Errorf("list projects: %w", err)
	}
	var demoProjectID int64
	found := false
	for _, p := range projects {
		if p.Name == "demo" {
			demoProjectID = p.ID
			found = true
			break
		}
	}
	if !found {
		return 0, fmt.Errorf("no %q project after SeedDemo", "demo")
	}

	tickets, err := st.TicketsByProject(ctx, demoProjectID)
	if err != nil {
		return 0, fmt.Errorf("tickets by project: %w", err)
	}
	if len(tickets) != 1 {
		return 0, fmt.Errorf("%d tickets under the demo project, want exactly 1", len(tickets))
	}
	return tickets[0].ID, nil
}

// selftestQuestionKindTitles are the six question-kind fixture titles
// SeedQuestionFixtures writes (internal/console/seed.go's seedQuestionText),
// one per design section 8 closed-set kind, in no particular order: a
// thread frame that shows every title has shown every kind's rendered
// control.
var selftestQuestionKindTitles = []string{
	"How should the greeting read?", // question
	"Approve the plan?",             // gate
	"Split this ticket?",            // split
	"Merge the PR?",                 // merge
	"Confirm the file perimeter",    // perimeter
	"Triage the review findings",    // review
}

// assertEveryQuestionKindRenders fails unless mainFrame -- one rendered
// #main thread frame -- contains every selftestQuestionKindTitles entry and
// exactly that many `<details class="q">` question groups (the same marker
// internal/console's own TestQuestionKindsRenderTheirControls splits on).
func assertEveryQuestionKindRenders(mainFrame string) error {
	for _, title := range selftestQuestionKindTitles {
		if !strings.Contains(mainFrame, title) {
			return fmt.Errorf("thread frame is missing the %q question group", title)
		}
	}
	groups := strings.Count(mainFrame, `<details class="q"`)
	if groups != len(selftestQuestionKindTitles) {
		return fmt.Errorf("thread frame has %d question groups, want exactly %d (one per closed-set kind)", groups, len(selftestQuestionKindTitles))
	}
	return nil
}

// selftestResumeE2E is Task 7's verify-by (design section 6.7, section 12
// row 7), run here as the suite that must never be skipped: the fake
// pipeline drives the fixture ticket from intake through planning on the
// real dispatcher, the fixture tracker, the fake runtime, and the real
// store; this answers its one planning question through the console's own
// live GET /stream plus real POST /draft then POST /send -- the same
// two-step a browser's chip click and send chord take, against a real
// network listener rather than an in-process recorder -- and asserts the
// resume advances the ticket to done, every state message appears in
// order in both the store and the live stream's own frames, and neither
// the stream's goroutine nor its bus subscription survives disconnecting it
// (design section 12 row 12; the technique mirrors Task 7's own
// internal/console/resume_e2e_test.go).
func selftestResumeE2E(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "zing-selftest-e2e")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	st, err := store.Open(ctx, filepath.Join(dir, "zing.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		return err
	}

	scripts, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		return fmt.Errorf("sub scripts fs: %w", err)
	}
	rt := runtime.NewFake(scripts)

	tr, err := tracker.NewFixture(fixtures.FS, "tickets.toml")
	if err != nil {
		return err
	}

	projectID, err := st.EnsureProject(ctx, store.Project{
		Name: "zing", RepoURL: "https://example.invalid/zing", LocalPath: dir, Tracker: "github",
	})
	if err != nil {
		return err
	}

	b := bus.New()
	d, err := zdispatch.New(st, tr, b, m, job.Registry(),
		[]zdispatch.Binding{{StoreProjectID: projectID, TrackerProject: "zing"}},
		zdispatch.Config{Interval: time.Millisecond, MaxParallel: 2, Owner: e2eOwner}, rt)
	if err != nil {
		return err
	}

	// The real console handler, over a real network listener (design
	// section 11, "stream": a live SSE connection needs one; an
	// httptest.NewRecorder cannot stream a still-running handler). This
	// suite never exercises /loglevel or /debug, so the Task 5/10 log
	// handler console.New now requires is a throwaway one over a discarded
	// sink, not the process's installed default (run's own, serve.go).
	logHandler := console.NewHandler(io.Discard, new(slog.LevelVar))
	srv, err := newSelftestConsoleServer(ctx, st, b, m, logHandler)
	if err != nil {
		return fmt.Errorf("start console server: %w", err)
	}
	defer srv.Close()

	ticketID, err := driveToOpenQuestion(ctx, d, st)
	if err != nil {
		return err
	}

	// The baseline is taken right before the /stream connection opens, with
	// the server and every other steady-state goroutine already running, so
	// the leak check below isolates this one connection's own resources
	// rather than the server's constant overhead (design section 11,
	// "stream" test seam).
	baseline := goruntime.NumGoroutine()

	streamResp, streamReader, cancelStream, err := getThreadStream(ctx, srv.URL, ticketID)
	if err != nil {
		return fmt.Errorf("open thread stream: %w", err)
	}
	frames := newSelftestFrameCollector(streamReader)
	closeStream := func() {
		cancelStream()
		_ = streamResp.Body.Close()
	}

	if err := answerFixtureQuestion(ctx, st, srv.URL, ticketID); err != nil {
		closeStream()
		return err
	}

	if err := driveToDone(ctx, d, st, ticketID); err != nil {
		closeStream()
		return err
	}

	if err := verifySelftestE2E(ctx, st, ticketID); err != nil {
		closeStream()
		return err
	}

	// Poll (bounded, not a fixed sleep) until the live stream itself has
	// shown every state transition, including "done": the ticket reaching
	// done in the store (asserted above) and the SSE handler's own
	// subsequent wake-and-patch are two different goroutines, so a fixed
	// assertion right after the store read could race a frame that simply
	// has not been flushed yet.
	if err := waitForSelftestFrames(frames, e2eWantStates); err != nil {
		closeStream()
		return err
	}

	closeStream()

	if err := waitForGoroutineBaseline(baseline); err != nil {
		return err
	}

	ch, unsub := b.Subscribe()
	defer unsub()
	b.Publish()
	select {
	case <-ch:
	case <-time.After(e2eFrameTimeout):
		return errors.New("e2e: a fresh subscriber saw no publish after the resume stream disconnected")
	}
	return nil
}

// driveToOpenQuestion ticks d, bounded by e2eMaxTicks, until the fixture
// ticket has appeared and is waiting on its planning question, and returns
// its id.
func driveToOpenQuestion(ctx context.Context, d *zdispatch.Dispatcher, st *store.Store) (int64, error) {
	var ticketID int64
	for i := range e2eMaxTicks {
		if err := d.Tick(ctx); err != nil {
			return 0, fmt.Errorf("tick %d: %w", i, err)
		}
		tickets, err := st.ListAllTickets(ctx)
		if err != nil {
			return 0, err
		}
		if len(tickets) == 0 {
			continue // intake has not run yet, or the fixture ticket is not there
		}
		if len(tickets) != 1 {
			return 0, fmt.Errorf("e2e: %d tickets after intake, want exactly 1", len(tickets))
		}
		ticketID = tickets[0].ID

		ticket, err := st.GetTicket(ctx, ticketID)
		if err != nil {
			return 0, err
		}
		if ticket.WaitingOn != nil && *ticket.WaitingOn == "questions" {
			return ticketID, nil
		}
	}
	if ticketID == 0 {
		return 0, fmt.Errorf("e2e: the fixture ticket never appeared within %d ticks", e2eMaxTicks)
	}
	return 0, fmt.Errorf("e2e: ticket %d never reached its planning question within %d ticks", ticketID, e2eMaxTicks)
}

// answerFixtureQuestion answers ticketID's one open planning question with
// its first offered option, through the console's own real POST /draft then
// POST /send (design section 6.7, section 11), the same two-step a
// browser's chip click and send chord take, and asserts the batch cleared
// the wait.
func answerFixtureQuestion(ctx context.Context, st *store.Store, base string, ticketID int64) error {
	open, err := st.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		return fmt.Errorf("questions by state: %w", err)
	}
	if len(open) != 1 {
		return fmt.Errorf("e2e: %d open questions on the fixture ticket, want exactly 1", len(open))
	}
	var payload response.QuestionPayload
	if unmarshalErr := json.Unmarshal(open[0].Payload, &payload); unmarshalErr != nil {
		return fmt.Errorf("unmarshal question payload: %w", unmarshalErr)
	}
	if len(payload.Options) == 0 {
		return fmt.Errorf("e2e: question %d has no options", open[0].ID)
	}
	chosen := payload.Options[0].Key

	draftBody := fmt.Sprintf(`{"ticket":%d,"question":%d,"option":%q}`, ticketID, open[0].ID, chosen)
	if postErr := postSelftestConsole(ctx, base, "/draft", draftBody); postErr != nil {
		return fmt.Errorf("draft answer for question %d: %w", open[0].ID, postErr)
	}
	sendBody := fmt.Sprintf(`{"ticket":%d}`, ticketID)
	if postErr := postSelftestConsole(ctx, base, "/send", sendBody); postErr != nil {
		return fmt.Errorf("send batch for ticket %d: %w", ticketID, postErr)
	}

	after, err := st.GetTicket(ctx, ticketID)
	if err != nil {
		return err
	}
	if after.WaitingOn != nil {
		return fmt.Errorf("e2e: ticket.WaitingOn after send = %q, want nil (the batch must clear the wait)", *after.WaitingOn)
	}
	return nil
}

// driveToDone keeps ticking d, bounded by e2eMaxTicks, until ticketID
// reaches state "done".
func driveToDone(ctx context.Context, d *zdispatch.Dispatcher, st *store.Store, ticketID int64) error {
	for i := range e2eMaxTicks {
		if err := d.Tick(ctx); err != nil {
			return fmt.Errorf("post-send tick %d: %w", i, err)
		}
		ticket, err := st.GetTicket(ctx, ticketID)
		if err != nil {
			return err
		}
		if ticket.State == "done" {
			return nil
		}
	}
	return fmt.Errorf("e2e: ticket did not reach done within %d ticks", e2eMaxTicks)
}

// newSelftestConsoleServer builds a real console.New handler bound to a
// reserved 127.0.0.1 listener, so its own real port -- not an arbitrary one
// -- is what console.New's mutation guard allowlists (mw.go, design section
// 6.14), and starts it, matching internal/console's own test helper
// technique (mw_test.go's newMutationTestServer).
func newSelftestConsoleServer(ctx context.Context, st *store.Store, b *bus.Broker, m *machine.Machine, logHandler *console.Handler) (*httptest.Server, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("reserve a listener: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("unexpected listener address type %T", ln.Addr())
	}

	handler := console.New(st, b, m, []string{"127.0.0.1"}, addr.Port, logHandler, nil, e2ePushToken)
	srv := httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		return nil, fmt.Errorf("close the placeholder listener: %w", err)
	}
	srv.Listener = ln
	srv.Start()
	return srv, nil
}

// getThreadStream opens GET /stream?view=thread&open=<ticketID> against
// base and returns the response, a buffered reader over its body, and its
// cancel func, matching internal/console/resume_e2e_test.go's own
// openThreadStream.
func getThreadStream(ctx context.Context, base string, ticketID int64) (*http.Response, *bufio.Reader, context.CancelFunc, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	v := url.Values{}
	v.Set("datastar", fmt.Sprintf(`{"view":"thread","open":%d,"project":0}`, ticketID))
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/stream?"+v.Encode(), http.NoBody)
	if err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("build GET /stream request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("GET /stream: %w", err)
	}
	return resp, bufio.NewReader(resp.Body), cancel, nil
}

// postSelftestConsole sends one JSON POST to base+path over a real network
// connection, carrying the Datastar-Request header and an Origin equal to
// base, the same shape a browser's chip click or send chord sends (design
// section 6.4, 6.14). Close is set so the connection does not linger in the
// client's keep-alive pool, which would otherwise show up as apparent
// goroutine growth in selftestResumeE2E's own leak check even though
// nothing leaked. It fails unless the handler reports 204.
func postSelftestConsole(ctx context.Context, base, path, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build POST %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Origin", base)
	req.Close = true

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("POST %s: status = %d, and reading the body failed: %w", path, resp.StatusCode, readErr)
		}
		return fmt.Errorf("POST %s: status = %d, body = %s", path, resp.StatusCode, respBody)
	}
	return nil
}

// readSelftestFrame reads one SSE frame from r, bounded by e2eFrameTimeout,
// matching internal/console/console_test.go's own readFrame.
func readSelftestFrame(r *bufio.Reader) (string, error) {
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var sb strings.Builder
		started := false
		for {
			line, err := r.ReadString('\n')
			if !started {
				if line == "\n" && err == nil {
					continue // a stray separator blank line before the frame starts
				}
				started = true
			}
			sb.WriteString(line)
			if err != nil {
				ch <- result{sb.String(), err}
				return
			}
			if line == "\n" {
				ch <- result{sb.String(), nil}
				return
			}
		}
	}()
	select {
	case res := <-ch:
		return res.text, res.err
	case <-time.After(e2eFrameTimeout):
		return "", errors.New("timed out waiting for an SSE frame")
	}
}

// readInitialSelftestFrames reads the three frames one /stream connect
// always sends, in the fixed order patchRegions writes them: #nav, then
// #main, then #rail (matching internal/console/stream_test.go's own
// readInitialFrames).
func readInitialSelftestFrames(r *bufio.Reader) (nav, main, rail string, err error) {
	if nav, err = readSelftestFrame(r); err != nil {
		return "", "", "", fmt.Errorf("read nav frame: %w", err)
	}
	if main, err = readSelftestFrame(r); err != nil {
		return "", "", "", fmt.Errorf("read main frame: %w", err)
	}
	if rail, err = readSelftestFrame(r); err != nil {
		return "", "", "", fmt.Errorf("read rail frame: %w", err)
	}
	return nav, main, rail, nil
}

// selftestFrameCollector accumulates every line read from a live /stream
// connection behind a mutex, so the main goroutine can poll its running
// text (String()) while the background reader is still live, matching
// internal/console/resume_e2e_test.go's own frameCollector.
type selftestFrameCollector struct {
	mu   sync.Mutex
	text strings.Builder
}

// newSelftestFrameCollector spawns one goroutine reading every SSE frame
// from r until it hits an error (the stream closing, on the caller's
// cancel).
func newSelftestFrameCollector(r *bufio.Reader) *selftestFrameCollector {
	c := &selftestFrameCollector{}
	go func() {
		for {
			line, err := r.ReadString('\n')
			c.mu.Lock()
			c.text.WriteString(line)
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return c
}

// String returns everything read so far, safe to call while the background
// reader is still running.
func (c *selftestFrameCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String()
}

// waitForSelftestFrames polls (bounded by e2eFrameTimeout, not a fixed
// sleep) until frames has shown a transition to every one of wantStates.
func waitForSelftestFrames(frames *selftestFrameCollector, wantStates []string) error {
	deadline := time.Now().Add(e2eFrameTimeout)
	for {
		if framesShowEveryStateSelftest(frames.String(), wantStates) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("live stream frames never showed every transition in %v; got:\n%s", wantStates, frames.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// framesShowEveryStateSelftest reports whether text contains a state
// separator (views.go's stateLine: "<from> -> <to>") for every state in
// wantStates, accepting either the raw or HTML-escaped arrow (templ
// auto-escapes text content).
func framesShowEveryStateSelftest(text string, wantStates []string) bool {
	for _, want := range wantStates {
		if !strings.Contains(text, "-&gt; "+want) && !strings.Contains(text, "-> "+want) {
			return false
		}
	}
	return true
}

// waitForGoroutineBaseline polls (bounded by e2eFrameTimeout) until the
// process's goroutine count has settled back to baseline, proving the
// disconnected stream's own goroutine is really gone.
func waitForGoroutineBaseline(baseline int) error {
	deadline := time.Now().Add(e2eFrameTimeout)
	for goruntime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			return fmt.Errorf("goroutine count did not settle back to the pre-stream baseline: got %d, baseline %d",
				goruntime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// verifySelftestE2E asserts the design section 11 end-to-end checkpoints
// once ticketID has reached done: the ordered state messages and exactly
// one question asked, answered, and resolved.
func verifySelftestE2E(ctx context.Context, st *store.Store, ticketID int64) error {
	msgs, err := st.ListMessages(ctx, ticketID)
	if err != nil {
		return err
	}

	var states []string
	var questions, answers, resolved int
	for i := range msgs {
		msg := &msgs[i]
		switch msg.Type {
		case "state":
			var sp response.StatePayload
			if err := json.Unmarshal(msg.Payload, &sp); err != nil {
				return fmt.Errorf("unmarshal state message %d: %w", msg.ID, err)
			}
			states = append(states, string(sp.To))
		case "question":
			questions++
		case "answer":
			answers++
		case "resolved":
			resolved++
		}
	}

	if !slices.Equal(states, e2eWantStates) {
		return fmt.Errorf("state messages = %v, want %v", states, e2eWantStates)
	}
	if questions != 1 {
		return fmt.Errorf("question messages = %d, want exactly 1", questions)
	}
	if answers != 1 {
		return fmt.Errorf("answer messages = %d, want exactly 1", answers)
	}
	if resolved != 1 {
		return fmt.Errorf("resolved messages = %d, want exactly 1", resolved)
	}
	return nil
}

// checkResponseTemplates renders every registered (job, outcome) pair's
// annotated template, failing on the first error (design section 6.10):
// a template exists for exactly the pairs the parser accepts.
// response.RegisteredPairs is internal/response's own single source of
// truth for that enumeration, so this can never drift from what Parse
// actually accepts.
func checkResponseTemplates() error {
	for _, p := range response.RegisteredPairs() {
		if _, err := response.RenderTemplate(p.Job, p.Outcome); err != nil {
			return fmt.Errorf("render %s/%s: %w", p.Job, p.Outcome, err)
		}
	}
	return nil
}

// checkResponseExamples parses and validates every example under fsys's
// examples/ directory (feature kind, no FS), design section 6.10. It reads
// through fsys, rather than response.ExampleFS directly, so a test can
// substitute a tampered filesystem without touching the real embedded
// files.
func checkResponseExamples(fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, "examples")
	if err != nil {
		return fmt.Errorf("list examples: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := "examples/" + e.Name()
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		doc, err := response.Parse(data)
		if err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		if errs := response.Validate(doc, response.ValidateContext{Kind: response.KindFeature}); len(errs) > 0 {
			return fmt.Errorf("validate %s: %w", name, errs[0])
		}
	}
	return nil
}
