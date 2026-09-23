// rail.go builds the #rail region's model (design section 6.11) from the
// open ticket's store reads plus the console's machine, and serves POST
// /side (design section 7.1), the inert side box's fixed reply. Task 9's
// own file: the design's section 5 perimeter was fixed before this task was
// scoped, the same way plan.go (Task 8) and log.go (Task 5) each added a
// file the original list did not name.
package console

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"zing/internal/console/templates"
	"zing/internal/response"
	"zing/internal/store"
)

const dash = "-"

// artifactSlotDefs maps the mock's seven labeled artifact-rail slots to the
// artifacts.type literal store.ListArtifacts groups by, and the machine
// state after which that artifact is normally produced (shown as "after
// <phase>" when the slot is absent).
//
// scenario, planreview, plan, finding, and verdict are internal/schemagen's
// own registered type names. "Decisions" names the planreview slot: design
// section 6.6 lists "the plan, scenarios, and decisions" as the gate kind's
// context, and response.FindingsResponse's own doc comment ("planreview and
// review, outcome ok") is what separates planreview's findings (the plan's
// decisions) from finding's (the review job's code-review report, this
// slot's "Review report"). design_doc and explainer have no producer in
// this or any merged package -- Packages 5 to 9 own them, the same carve-out
// design section 4 states for gate, perimeter, review, split, and merge:
// "No real producers ... fixtures seed them." Their AfterPhase is this
// file's own reasonable placement (documented here, not a producer
// contract), since no plan or design text pins one: a design doc as a
// polished companion to the stored plan (after planning), an explainer as
// the plain-language summary a shipped change carries (after shipping).
var artifactSlotDefs = []struct {
	Label      string
	Type       string
	AfterPhase string
}{
	{"Scenarios", "scenario", string(response.TicketStatePlanning)},
	{"Decisions", "planreview", string(response.TicketStatePlanning)},
	{"Plan", "plan", string(response.TicketStatePlanning)},
	{"Design doc", "design_doc", string(response.TicketStatePlanning)},
	{"Explainer", "explainer", string(response.TicketStateShipping)},
	{"Review report", "finding", string(response.TicketStateReviewing)},
	{"Judge verdict", "verdict", string(response.TicketStateJudging)},
}

// railComponent builds the #rail region for the current view (design
// section 6.3, 6.11): the rail's real content when view is thread and open
// names a real ticket, the same empty placeholder every other frame (Task
// 3's Rail(nil) shell), so #rail is always patched (design section 6.3:
// "leaving a thread clears the old rail").
func (c *console) railComponent(ctx context.Context, view string, open int64) (templ.Component, error) {
	if view != viewThread || open <= 0 {
		return templates.Rail(nil), nil
	}
	model, err := c.buildRailModel(ctx, open)
	if err != nil {
		return nil, err
	}
	return templates.Rail(model), nil // model is nil when open names no ticket; still the empty shell
}

// buildRailModel reads ticketID's ticket, artifacts, sessions, and runs and
// assembles the rail's full model (design section 6.11). It returns nil,
// nil, matching store.GetTicket's own sql.ErrNoRows shape, when open names
// no ticket -- the same "not there" case threadComponent (views.go) already
// handles for #main.
func (c *console) buildRailModel(ctx context.Context, ticketID int64) (*templates.RailModel, error) {
	ticket, err := c.store.GetTicket(ctx, ticketID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil //nolint:nilnil // "no such ticket" is a legitimate result, not an error
		}
		return nil, fmt.Errorf("console: rail: get ticket %d: %w", ticketID, err)
	}

	artifacts, err := c.buildArtifactsRail(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	run, err := c.buildRunRail(ctx, ticketID)
	if err != nil {
		return nil, err
	}

	return &templates.RailModel{
		Phase:     c.buildPhaseRail(ticket),
		Artifacts: artifacts,
		Run:       run,
	}, nil
}

// buildPhaseRail draws the console's machine.States.Order as dots (design
// section 6.11): every state before ticket.State is done, ticket.State
// itself is now (Waiting set when ticket.WaitingOn is non-nil), and every
// state after is upcoming. A nil console.machine (every test that does not
// exercise the rail) or a ticket state absent from Order (the two terminal
// states outside the pipeline, escalated and abandoned, which
// machine.toml's states.order does not list) both yield an all-upcoming
// (or, with no machine, empty) row rather than a panic or a guessed "now".
func (c *console) buildPhaseRail(ticket store.Ticket) []templates.PhaseDot {
	if c.machine == nil {
		return nil
	}
	order := c.machine.States.Order

	nowIndex := -1
	for i, s := range order {
		if s == ticket.State {
			nowIndex = i
			break
		}
	}

	dots := make([]templates.PhaseDot, len(order))
	for i, s := range order {
		dot := templates.PhaseDot{State: s, Status: "upcoming"}
		switch {
		case nowIndex < 0:
			// ticket.State names no pipeline phase (escalated, abandoned):
			// leave every dot upcoming rather than guess.
		case i < nowIndex:
			dot.Status = "done"
		case i == nowIndex:
			dot.Status = "now"
			dot.Waiting = ticket.WaitingOn != nil
		}
		dots[i] = dot
	}
	return dots
}

// buildArtifactsRail groups ticketID's artifacts by type into the mock's
// seven labeled slots (design section 6.11, artifactSlotDefs above). Each
// slot shows its newest version, or "after <phase>" when the ticket has no
// artifact of that type.
func (c *console) buildArtifactsRail(ctx context.Context, ticketID int64) ([]templates.ArtifactSlot, error) {
	all, err := c.store.ListArtifacts(ctx, ticketID)
	if err != nil {
		return nil, fmt.Errorf("console: rail: list artifacts for ticket %d: %w", ticketID, err)
	}

	// store.ListArtifacts orders by type, version DESC, id (design section
	// 7.2), so the first row seen for a type is already that type's newest
	// version; a later row for the same type is an older one and is
	// skipped.
	newest := make(map[string]store.Artifact, len(all))
	for _, a := range all {
		if _, ok := newest[a.Type]; !ok {
			newest[a.Type] = a
		}
	}

	slots := make([]templates.ArtifactSlot, 0, len(artifactSlotDefs))
	for _, def := range artifactSlotDefs {
		slot := templates.ArtifactSlot{Label: def.Label, AfterPhase: def.AfterPhase}
		if a, ok := newest[def.Type]; ok {
			slot.Present = true
			slot.Version = a.Version
			slot.PayloadText = prettyPayload(a.Payload)
		}
		slots = append(slots, slot)
	}
	return slots, nil
}

// prettyPayload indents raw's JSON for the artifact disclosure's <pre>
// block, falling back to the raw bytes unmodified on a malformed payload
// (unreachable in practice: every stored artifact already passed
// InsertArtifact's schema validation), so a render never fails over
// cosmetic indentation.
func prettyPayload(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// buildRunRail renders the newest session's newest run (design section
// 6.11): SessionsForTicket and RunsForTicket both order by id (design
// section 7.2), so the newest session is the last element SessionsForTicket
// returns, and the newest run is the last RunsForTicket row belonging to
// that session (the greatest id; among a session's runs the greatest id
// always carries the greatest turn too, since turn only advances within a
// session's own monotonically increasing run ids).
//
// Attempts reads Session.Resumes+1 rather than the run's own Turn: Resumes
// counts how many times this session's asking run was resumed (design
// section 6.7's "the asking run resumes once" is exactly one such attempt),
// which is the rail's "how many tries has this ticket's current job taken"
// question; Turn instead numbers a turn within one long session and would
// undercount a resumed session whose session-level retry the console cannot
// otherwise show. This is a judgment call the design section 6.11 leaves
// open ("your reasonable call, documented").
func (c *console) buildRunRail(ctx context.Context, ticketID int64) (templates.RunRail, error) {
	sessions, err := c.store.SessionsForTicket(ctx, ticketID)
	if err != nil {
		return templates.RunRail{}, fmt.Errorf("console: rail: sessions for ticket %d: %w", ticketID, err)
	}
	if len(sessions) == 0 {
		return templates.RunRail{Model: dash, AgentTime: dash, Attempts: dash, Worktree: dash, Branch: dash}, nil
	}
	newestSession := sessions[len(sessions)-1]

	runs, err := c.store.RunsForTicket(ctx, ticketID)
	if err != nil {
		return templates.RunRail{}, fmt.Errorf("console: rail: runs for ticket %d: %w", ticketID, err)
	}
	var newestRun store.Run
	haveRun := false
	for _, r := range runs {
		if r.SessionID == newestSession.ID {
			newestRun = r // runs is id-ascending, so the last match is the newest
			haveRun = true
		}
	}

	run := templates.RunRail{
		Attempts: strconv.Itoa(newestSession.Resumes + 1),
		Worktree: dash, // arrives with Package 5 (design section 6.11)
		Branch:   dash, // arrives with Package 5 (design section 6.11)
	}
	if !haveRun {
		run.Model, run.AgentTime = dash, dash
		return run, nil
	}
	if newestRun.Model != nil {
		run.Model = *newestRun.Model
	} else {
		run.Model = dash
	}
	if newestRun.AgentSeconds != nil {
		run.AgentTime = (time.Duration(*newestRun.AgentSeconds) * time.Second).String()
	} else {
		run.AgentTime = dash
	}
	return run, nil
}

// ---- POST /side (design section 6.11, 7.1) ---------------------------

// sideReplyText is POST /side's one fixed, inert reply (design section
// 6.11: "a fixed inert reply ('The side agent arrives in Package 10.')").
// It never varies with the request, since the side agent itself is Package
// 10's job, not this package's.
const sideReplyText = "The side agent arrives in Package 10."

// maxSideBodyBytes bounds POST /side's request body, the same cap
// answer.go's POST /draft uses (design section 6.7's 64 KiB body cap; this
// route shares the shape, not the section, since section 6.7 is the
// composer's own, but the same defensive bound belongs on every mutation
// route's body).
const maxSideBodyBytes = 64 << 10

// sideRequest is POST /side's body: the ticket it was opened from (unused
// today -- the reply never varies -- but carried so Package 10's real side
// job can read it without a wire-format change) and the free text typed
// into the side box.
type sideRequest struct {
	Ticket int64  `json:"ticket"`
	Text   string `json:"text"`
}

// handleSide is POST /side (design section 6.11, 7.1): decode the body
// strictly and bounded, matching answer.go's other mutation handlers, then
// answer with the one fixed reply fragment rendered as this response's
// entire body -- not a 204, and not a bus.Publish -- since the side box
// never touches a run and never posts to the thread (design section 6.11)
// and so has nothing for the live /stream to pick up. console.js's postSide
// reads this response directly and swaps it into the rail's #side-reply
// element itself.
func (c *console) handleSide(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSideBodyBytes)

	var req sideRequest
	if err := decodeStrict(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if len([]rune(req.Text)) > maxDraftTextLen {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var buf bytes.Buffer
	if err := templates.SideReply(sideReplyText).Render(r.Context(), &buf); err != nil {
		slog.Error("console: render side reply", "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeHTML)
	if _, err := w.Write(buf.Bytes()); err != nil {
		slog.Error("console: write side reply", "err", err)
	}
}
