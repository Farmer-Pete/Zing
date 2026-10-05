// Package console serves the console (design section 6.3): the shell page,
// one live GET /stream per tab that patches the #nav, #main, and #rail
// regions, and the composer's POST /draft, /send, and /read (design section
// 6.7, 6.8). Every page and fragment renders through the templ components
// in internal/console/templates (design section 6.2); there is no
// html/template use in this package.
package console

import (
	_ "embed"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"zing/internal/bus"
	"zing/internal/machine"
	"zing/internal/response"
	"zing/internal/store"
	"zing/internal/tracker"
)

// contentTypeJS and contentTypeJSON are the MIME types the vendored and
// authored static assets are served with; named once so goconst has
// nothing to flag.
const (
	contentTypeJS   = "text/javascript"
	contentTypeJSON = "application/json"
)

// nonStreamWriteDeadline bounds how long one of the non-streaming routes
// has to finish writing its response (design section 6.10). It must never
// apply to the SSE routes, which hold the connection open far past any such
// bound on purpose (datastar skill, "Long-lived streams").
const nonStreamWriteDeadline = 5 * time.Second

// streamHeartbeatInterval is the interval New sets console.streamHeartbeat
// to: an upper bound on how long a missed wake -- a dropped publish, a
// wedged write recovering, a reconnect that never arrived -- can leave the
// page showing stale content before the next full re-render corrects it.
// keyboard.mjs's STREAM_IDLE_MS (45s) is three of these beats, so three
// missed beats in a row, not one slow one, is what finally shows the stale
// marker.
const streamHeartbeatInterval = 15 * time.Second

// streamFrameWriteTimeout is the interval New sets console.streamWriteTimeout
// to: how long one region's write on /stream may block before
// armFrameWriteDeadline's deadline fails it and handleStream ends the
// stream, rather than leaving a wedged write to swallow every later wake
// forever.
const streamFrameWriteTimeout = 10 * time.Second

// The five public static assets (design section 5, 12): the vendored
// Datastar bundle (Package 3), the vendored mermaid.js (static/ASSETS.md
// records its source, version, and digest), and three assets authored in
// this repo (console.js, keyboard.mjs, keys.json are Task 1 skeletons or
// placeholders; Task 4 fills them in for real). Each is embedded by its
// own exact path, never as a directory tree, so the mux below can register
// an explicit allowlist: console.test.js, package.json, and ASSETS.md have
// no route and so 404, the same as any other unlisted path under /static/.
//
//go:embed static/datastar.js
var datastarJS []byte

//go:embed static/mermaid.js
var mermaidJS []byte

//go:embed static/console.js
var consoleJS []byte

//go:embed static/keyboard.mjs
var keyboardMJS []byte

//go:embed static/keys.json
var keysJSON []byte

// console holds the read access every handler needs: the store to render
// from, the bus every SSE stream subscribes to for its wake-up signal, the
// machine (nilable) the rail's Phase section reads States.Order from
// (design section 6.1, 6.11), and log, the Task 5 slog.Handler (design
// section 6.12) cmd/zing installs as slog's default and this package reads
// from and mutates live: the Log rail's tail (rail.go's buildLogRail) and
// POST /loglevel and /debug (control.go). A nil machine (every test that
// does not exercise the rail passes one) renders no phase dots rather than
// panicking (rail.go's buildPhaseRail); log has no such nil case, since
// every caller of New, including every test, now builds one (design
// section 12, Task 10). push and pushToken back GET /push/key and POST
// /push/subscribe (push.go, design section 6.13): push is nilable the same
// way machine is, for a test that never exercises those two routes. floor
// is the configured review.floor (design section 4.4, 7, D8): the gate's
// context region (views.go's loadFindings) keeps only a stored planreview
// artifact's findings whose severity ranks strictly above it, the same
// threshold job/planning.go's own floor split uses, without either package
// importing the other. sandboxReason is sandbox.Set.FirstUnavailable's own
// result (design section 9.2, Task 15; PKG9-PLAN.md section 4.7): empty
// when every profile some machine.toml job names is available, in which
// case the nav region renders no indicator; otherwise it renders the
// sandbox-off span with this "<name>: <reason>" text as the title.
type console struct {
	store         *store.Store
	bus           *bus.Broker
	machine       *machine.Machine
	log           *Handler
	push          PushKeys
	pushToken     string
	floor         response.Severity
	sandboxReason string
	// tracker and user back POST /projects/{id}/pickup's manual intake
	// (pickup.go, PKG9-PLAN.md D29): tracker calls Issue against a project's
	// own tracker project name (looked up by id through c.store), and user
	// is cfg.User, the Zing-acts-for identity the pickup comment names --
	// the same value ensureBindings (cmd/zing/serve.go) already sets on
	// every dispatch.Binding.User for intake's own pickup comment. Nilable
	// the way push and machine already are: a test that never exercises
	// POST /projects/{id}/pickup passes nil and "".
	tracker tracker.Tracker
	user    string

	// run backs POST /tickets/{id}/sandbox-run (sandboxrun.go): the owner's
	// "run a command in this ticket's sandbox" console action. job.
	// TicketCommands is the real TicketRunner, built in cmd/zing/serve.go
	// with the same CommandRunner value the dispatcher's CHECK step uses,
	// so the console can never drift from what CHECK itself runs. Nilable
	// the way push and tracker already are: a nil run answers that route
	// with 503 "sandbox runs are not available", and every test that does
	// not itself exercise the route passes nil.
	run TicketRunner

	// startedAt is when New built this console (bug fix: the Log rail's
	// empty state read as "No log lines yet." after every `zing serve`
	// restart, since log.go's ring is in memory and a restart always starts
	// it empty, which looked broken rather than merely quiet. The ring
	// itself is not persisted -- buildLogRail (rail.go) uses startedAt only
	// to say since when there is nothing to show).
	startedAt time.Time

	// streamHeartbeat is how often handleStream's select loop re-renders
	// and patches all four regions with no bus wake at all (bug fix: a
	// stream that goes silent without closing -- a half-open socket, a
	// hidden-tab pause, or a wedged server write -- left the page showing
	// stale content with no stale marker, since the client only notices a
	// finished or error event, neither of which a silently stalled stream
	// ever produces). New sets it to streamHeartbeatInterval; zero (a bare
	// struct literal, every test that does not itself exercise the beat)
	// disables it, so handleStream's ticker case never fires.
	streamHeartbeat time.Duration

	// streamWriteTimeout bounds how long patchRegions' armFrameWriteDeadline
	// gives each of the four region writes on /stream (bug fix: handleStream
	// clears the write deadline entirely on connect (stream.go's
	// SetWriteDeadline(time.Time{})), so a write that wedges -- a half-open
	// socket the kernel has not yet noticed -- blocked forever and the bus's
	// one-slot buffer then dropped every later wake). New sets it to
	// streamFrameWriteTimeout; zero (a bare struct literal, every test that
	// does not itself exercise the deadline) disables it, so
	// armFrameWriteDeadline arms nothing and a region write can still block
	// as before.
	streamWriteTimeout time.Duration
}

// New builds the console and returns it as an http.Handler:
//
//	GET  /                     the shell page
//	GET  /stream                the one live SSE stream per tab (design section 6.3)
//	POST /draft                 save one draft answer or reply (design section 6.7)
//	POST /send                  send the ticket's drafted batch (design section 6.7)
//	POST /read                  mark one message read (design section 6.8)
//	POST /loglevel               change the runtime log level (design section 6.12, 7.1)
//	POST /debug                  toggle one ticket's per-ticket debug override (design section 6.12, 7.1)
//	POST /side                  the inert side box's fixed reply (design section 6.11, 7.1)
//	POST /stop                  stop everything, or one ticket; no keyboard key offers this yet (design section 6.11, 7.1)
//	POST /projects/{id}/pickup  manual intake: pick up one issue by number (PKG9-PLAN.md D29)
//	POST /tickets/{id}/edit     the owner edits a sealed scenario, a sealed plan's task, or the ticket body
//	POST /tickets/{id}/sandbox-run  run one command in a ticket's worktree as CHECK runs it (loopback only)
//	GET  /runs/{id}/{kind}      one run's final message, stderr, or transcript, as plain text
//	GET  /push/key               the VAPID public key (design section 6.13, 7.1)
//	POST /push/subscribe        store one push subscription (design section 6.13, 7.1)
//	GET  /static/datastar.js    the vendored Datastar bundle
//	GET  /static/mermaid.js     the vendored mermaid bundle
//	GET  /static/console.js     the console's DOM wiring (Task 1 skeleton)
//	GET  /static/keyboard.mjs   the console's pure keyboard logic (Task 1 skeleton)
//	GET  /static/keys.json      the keyboard binding table (Task 1 placeholder)
//
// /static/ is an explicit allowlist of exactly those five assets (design
// section 5, 12): every other path, including console.test.js, package.json,
// and ASSETS.md, has no registered route and so 404s from the mux itself.
//
// hosts and port build the mutation guard's Host allowlist (mw.go, design
// section 6.14): every entry in hosts, plus localhost and 127.0.0.1, each
// at port. cmd/zing/serve.go builds hosts from every resolved console.bind
// authority plus Console.AllowedHosts (design section 6.14, Task 11).
//
// log is the Task 5 slog.Handler (log.go): cmd/zing builds it, seeds its
// LevelVar from settings.log_level, and installs it as slog's default
// before calling New (design section 6.12, cmd/zing/serve.go), so this
// same instance backs both the process's own logging and the console's
// live level control, per-ticket debug toggle, and Log rail tail.
//
// push and pushToken back GET /push/key and POST /push/subscribe (push.go,
// design section 6.13): push may be nil for a caller (most tests) that
// never exercises those two routes.
//
// floor is the configured review.floor (design section 4.4, 7, D8, Task
// 11): cmd/zing's serve and selftest each parse it once (response.
// ParseSeverity(cfg.Review.Floor)) and pass the same value here and into
// job.Deps.Floor, so the gate's context region and the planning handler's
// floor split agree on one threshold.
//
// sandboxReason is sandbox.Set.FirstUnavailable(used) (design section 5.4,
// 9.2, Task 15; PKG9-PLAN.md section 4.7): empty means every profile some
// machine.toml job names is available. serve passes its real Set's own
// FirstUnavailable result, over the jobs machine.toml actually names a
// profile for; selftest passes sandbox.Off().Reason(), since its
// dispatcher always runs unwrapped.
//
// tr and user back POST /projects/{id}/pickup (PKG9-PLAN.md D29): tr is the
// production tracker cmd/zing/serve.go already builds (productionTracker),
// and user is cfg.User. Either may be left zero (nil, "") by a caller that
// never exercises that one route.
//
// run backs POST /tickets/{id}/sandbox-run (sandboxrun.go): cmd/zing/serve.go
// passes job.TicketCommands{..., Commands: cmds}, the same cmds value the
// dispatcher's CHECK step runs with. A nil run (every other caller: selftest,
// most tests) makes that route answer 503 "sandbox runs are not available".
//
// The returned handler is a *http.ServeMux, plain HTTP/1.1, with no timeouts
// of its own; cmd/zing wraps it in an http.Server with the drain-aware
// BaseContext and shutdown sequence (design section 6.14, cmd/zing/serve.go).
func New(st *store.Store, b *bus.Broker, m *machine.Machine, hosts []string, port int, log *Handler, push PushKeys, pushToken string, floor response.Severity, sandboxReason string, tr tracker.Tracker, user string, run TicketRunner) http.Handler {
	c := &console{
		store: st, bus: b, machine: m, log: log, push: push, pushToken: pushToken,
		floor: floor, sandboxReason: sandboxReason, tracker: tr, user: user,
		run:                run,
		startedAt:          time.Now(),
		streamHeartbeat:    streamHeartbeatInterval,
		streamWriteTimeout: streamFrameWriteTimeout,
	}
	guard := newMutationGuard(port, append(append([]string{}, hosts...), "localhost", "127.0.0.1")...)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", withWriteDeadline(guard.requireAllowedHost(c.handleIndex)))
	mux.HandleFunc("GET /stream", guard.requireAllowedHost(c.handleStream)) // streaming: no write deadline
	mux.HandleFunc("POST /draft", withWriteDeadline(guard.requireSameOrigin(c.handleDraft)))
	mux.HandleFunc("POST /send", withWriteDeadline(guard.requireSameOrigin(c.handleSend)))
	mux.HandleFunc("POST /read", withWriteDeadline(guard.requireSameOrigin(c.handleRead)))
	mux.HandleFunc("POST /loglevel", withWriteDeadline(guard.requireSameOrigin(c.handleLogLevel)))
	mux.HandleFunc("POST /debug", withWriteDeadline(guard.requireSameOrigin(c.handleDebug)))
	mux.HandleFunc("POST /side", withWriteDeadline(guard.requireSameOrigin(c.handleSide)))
	mux.HandleFunc("POST /stop", withWriteDeadline(guard.requireSameOrigin(c.handleStop)))
	mux.HandleFunc("POST /projects/{id}/pickup", withWriteDeadline(guard.requireSameOrigin(c.handlePickup)))
	mux.HandleFunc("POST /tickets/{id}/edit", withWriteDeadline(guard.requireSameOrigin(c.handleOwnerEdit)))
	mux.HandleFunc("POST /tickets/{id}/sandbox-run", requireLoopback(guard.requireSameOrigin(c.handleSandboxRun))) // long-running: no write deadline
	mux.HandleFunc("GET /runs/{id}/{kind}", withWriteDeadline(guard.requireAllowedHost(c.handleRunFile)))
	mux.HandleFunc("GET /push/key", withWriteDeadline(c.handlePushKey))
	// POST /push/subscribe is token-only (push.go's checkPushToken), not
	// behind the same-origin guard: a phone subscribing is authenticated by
	// the bearer token it was handed, not by browser same-origin, and its
	// MagicDNS host need not be in allowed_hosts. This matches GET
	// /push/key, already token-only for the same reason (design section
	// 6.13).
	mux.HandleFunc("POST /push/subscribe", withWriteDeadline(c.handlePushSubscribe))
	mux.HandleFunc("GET /static/datastar.js", withWriteDeadline(staticAsset(datastarJS, contentTypeJS)))
	mux.HandleFunc("GET /static/mermaid.js", withWriteDeadline(staticAsset(mermaidJS, contentTypeJS)))
	mux.HandleFunc("GET /static/console.js", withWriteDeadline(staticAsset(consoleJS, contentTypeJS)))
	mux.HandleFunc("GET /static/keyboard.mjs", withWriteDeadline(staticAsset(keyboardMJS, contentTypeJS)))
	mux.HandleFunc("GET /static/keys.json", withWriteDeadline(staticAsset(keysJSON, contentTypeJSON)))
	return mux
}

// withWriteDeadline wraps a non-streaming handler with a per-request write
// deadline set through http.ResponseController, so a stalled write cannot
// hang a connection open indefinitely (design section 6.10). It must wrap
// only the non-streaming routes: New (above) is the one place that knows
// which routes stream and which do not, so /stream never gets wrapped
// here, matching its own SetWriteDeadline(time.Time{}) call that clears any
// deadline before it starts writing.
//
// http.ErrNotSupported is not fatal here, unlike in the streaming handler: it
// means w does not implement the optional deadline interface at all, which
// on a real connection never happens (net/http's own ResponseWriter always
// does) and only arises when a handler is driven directly against an
// httptest.ResponseRecorder with no real connection behind it, such as
// cmd/zing's in-process selftest e2e POST /answer (design section 11,
// "cmd/zing" fix 7). A request driven that way has nothing to time out
// against in the first place, so skipping the deadline and serving the
// request anyway is correct, not a silent downgrade of real traffic.
func withWriteDeadline(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Now().Add(nonStreamWriteDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		next(w, r)
	}
}

// staticAsset returns a handler serving one embedded static asset with a
// fixed content type. Every asset served this way is either vendored and
// reviewed once, not fetched at runtime (datastar.js, mermaid.js; design
// section 0, dependency set, and static/ASSETS.md), or authored in this
// repo (console.js, keyboard.mjs, keys.json).
func staticAsset(body []byte, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if _, err := w.Write(body); err != nil {
			slog.Error("console: write static asset", "content_type", contentType, "err", err)
		}
	}
}
