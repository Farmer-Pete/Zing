package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// e2eMaxTicks bounds selftestE2E's tick loop: enough ticks for intake plus
// one handler call per pipeline transition (design section 7.1: queued,
// planning x2, building, reviewing, judging, shipping is 7 handler calls),
// with generous headroom, so a stuck dispatcher fails the selftest promptly
// instead of hanging.
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

// selftestE2E proves the design section 11 end-to-end suite: a fixture
// ticket goes from intake to done through the real dispatcher, the real
// store, the fixture tracker, and the fake runtime, entirely on a temp-dir
// store (never ~/.zing). It drives Tick directly in a bounded loop (no
// timer, no network port) so the run is deterministic, answers the one
// planning question through store.AnswerQuestion as soon as the ticket
// waits on it, and asserts the ordered state messages, that exactly one
// question was asked, answered, and resolved, and the terminal done state.
func selftestE2E(ctx context.Context) error {
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

	// The real console handler, driven in-process through
	// httptest.NewRecorder (no network port): section 11's e2e suite
	// answers through POST /draft then POST /send exactly as the browser's
	// chip click and send chord would, not through store.SaveDraft or
	// store.SendBatch directly, so this suite also proves the console's own
	// composer path end to end (design section 11, "cmd/zing" fix 7;
	// design section 6.7). e2eConsoleHost and e2eConsolePort are a fixed,
	// made-up bind, never actually listened on, that only needs to satisfy
	// the mutation guard (mw.go, design section 6.14) the requests below
	// carry matching Host and Origin headers for. This suite never exercises
	// /loglevel or /debug, so the Task 5/10 log handler console.New now
	// requires is a throwaway one over a discarded sink, not the process's
	// installed default (run's own, serve.go).
	logHandler := console.NewHandler(io.Discard, new(slog.LevelVar))
	consoleHandler := console.New(st, b, m, e2eConsoleHost, e2eConsolePort, logHandler)

	var ticketID int64
	var answered int

	for i := range e2eMaxTicks {
		if err := d.Tick(ctx); err != nil {
			return fmt.Errorf("tick %d: %w", i, err)
		}

		if ticketID == 0 {
			tickets, err := st.ListAllTickets(ctx)
			if err != nil {
				return err
			}
			if len(tickets) == 0 {
				continue // intake has not run yet, or the fixture ticket is not there
			}
			if len(tickets) != 1 {
				return fmt.Errorf("e2e: %d tickets after intake, want exactly 1", len(tickets))
			}
			ticketID = tickets[0].ID
		}

		ticket, err := st.GetTicket(ctx, ticketID)
		if err != nil {
			return err
		}

		if ticket.WaitingOn != nil && *ticket.WaitingOn == "questions" {
			n, err := answerOpenQuestions(ctx, st, consoleHandler, ticketID)
			if err != nil {
				return err
			}
			answered += n
		}

		if ticket.State == "done" {
			return verifySelftestE2E(ctx, st, ticketID, answered)
		}
	}
	return fmt.Errorf("e2e: ticket did not reach done within %d ticks", e2eMaxTicks)
}

// e2eConsoleHost and e2eConsolePort are the fixed authority
// selftestE2E's in-process requests present as Host and Origin, so they
// pass the mutation guard (mw.go, design section 6.14) without this suite
// needing a real network listener.
const (
	e2eConsoleHost = "127.0.0.1"
	e2eConsolePort = 7420
)

// answerOpenQuestions answers every question ticketID has open, each with
// its first offered option, by drafting then sending through the real
// console composer handlers in-process (design section 6.7, section 11),
// and returns how many it answered.
func answerOpenQuestions(ctx context.Context, st *store.Store, consoleHandler http.Handler, ticketID int64) (int, error) {
	open, err := st.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		return 0, fmt.Errorf("questions by state: %w", err)
	}
	for i := range open {
		q := &open[i]
		var payload response.QuestionPayload
		if err := json.Unmarshal(q.Payload, &payload); err != nil {
			return 0, fmt.Errorf("unmarshal question %d payload: %w", q.ID, err)
		}
		if len(payload.Options) == 0 {
			return 0, fmt.Errorf("question %d has no options", q.ID)
		}
		draftBody := fmt.Sprintf(`{"ticket":%d,"question":%d,"option":%q}`, ticketID, q.ID, payload.Options[0].Key)
		if err := postConsole(ctx, consoleHandler, "/draft", draftBody); err != nil {
			return 0, fmt.Errorf("draft answer for question %d: %w", q.ID, err)
		}
	}
	if len(open) > 0 {
		sendBody := fmt.Sprintf(`{"ticket":%d}`, ticketID)
		if err := postConsole(ctx, consoleHandler, "/send", sendBody); err != nil {
			return 0, fmt.Errorf("send batch for ticket %d: %w", ticketID, err)
		}
	}
	return len(open), nil
}

// postConsole sends one JSON POST through consoleHandler's real route
// (path), in-process: an http.Request carrying the Datastar-Request header
// and a Host/Origin authority matching e2eConsoleHost:e2eConsolePort, the
// same shape a browser's chip click or send chord sends (design section
// 6.4, 6.14), served directly to an httptest.NewRecorder rather than over a
// network port. It fails unless the handler reports 204, the same contract
// answer_test.go's own composer tests assert.
func postConsole(ctx context.Context, consoleHandler http.Handler, path, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build POST %s request: %w", path, err)
	}
	authority := fmt.Sprintf("%s:%d", e2eConsoleHost, e2eConsolePort)
	req.Host = authority
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Origin", "http://"+authority)

	rec := httptest.NewRecorder()
	consoleHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		return fmt.Errorf("POST %s: status = %d, body = %s", path, rec.Code, rec.Body.String())
	}
	return nil
}

// verifySelftestE2E asserts the design section 11 end-to-end checkpoints
// once ticketID has reached done: the ordered state messages, exactly one
// question asked, answered, and resolved, and that answered (the count
// selftestE2E's loop accumulated through AnswerQuestion) is exactly 1.
func verifySelftestE2E(ctx context.Context, st *store.Store, ticketID int64, answered int) error {
	if answered != 1 {
		return fmt.Errorf("answered %d questions across the run, want exactly 1", answered)
	}

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
