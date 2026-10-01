// live_review_test.go is the opt-in live harness for the real reviewing
// state machine's own ROUND (PKG9-PLAN.md section 6.2, 19.2 task 12): the
// one place this repository proves a real review round against the real,
// pinned claude CLI under the readonly sandbox profile, rather than the
// fake runtime every other test in this package drives.
//
// TestLiveReview spends the owner's real Claude usage. It never runs by
// default, in CI, or from any other test: it checks ZING_LIVE_CLI itself
// and skips unless it is exactly "1", and only on macOS (the sandbox is
// darwin-only, design N9, section 5). It reuses live_build_test.go's own
// gate (liveBuildSkipReason, liveClaudeOAuthToken, newLiveStore): a review
// round answers to the same gate a build run does, and this file adds
// nothing state-specific to either. Run it explicitly, deliberately:
//
//	ZING_LIVE_CLI=1 go test ./cmd/zing -run TestLiveReview -v -timeout 20m
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	goruntime "runtime"
	"testing"
	"time"

	zing "zing"
	"zing/fixtures"
	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// errLiveReviewGitHub is liveReviewGitHub's own shared error: one review
// round never reaches GitHub, so every method of it returns this.
var errLiveReviewGitHub = errors.New("live review: unexpected GitHub call")

// liveReviewGitHub is a never-called orchestrator.GitHub (orchestrator.New's
// required parameter): one review round never reaches GitHub.
type liveReviewGitHub struct{}

func (liveReviewGitHub) RepoDefaultBranch(context.Context, string, string) (branch string, err error) {
	return "", errLiveReviewGitHub
}

func (liveReviewGitHub) RequiredChecks(context.Context, string, string, string) (checks []string, err error) {
	return nil, errLiveReviewGitHub
}

func (liveReviewGitHub) CreateDraftPR(context.Context, string, string, string, string, string, string) (prURL string, number int, err error) {
	return "", 0, errLiveReviewGitHub
}

func (liveReviewGitHub) FindPRByHead(context.Context, string, string, string, string) (prURL string, number int, ok bool, err error) {
	return "", 0, false, errLiveReviewGitHub
}

// liveReviewRepoName is this harness's own gitfixture repo and store
// project name: distinct from live_build_test.go's own "greeter" fixture
// (goconst: a second "greeter" literal here would push that string past
// golangci-lint's own repeat threshold).
const liveReviewRepoName = "greeter-review"

// liveReviewGreetGoBase is the declared file (its own task 2 entry in
// fixtures/scripts/planning/2.xml's own <delivery><files>, reused below as
// this harness's own stored plan) this harness plants its defect into:
// this correct version lands on main.
const liveReviewGreetGoBase = `package greeter

import "strings"

// Greet returns a friendly greeting for name.
func Greet(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Hello, friend!"
	}
	return "Hello, " + name + "!"
}
`

// liveReviewGreetGoDefect is the ticket branch's own one commit: GreetAll's
// own off-by-one (<= instead of <) panics with index out of range on its
// very last name, entirely inside the diff ROUND reads against main.
const liveReviewGreetGoDefect = liveReviewGreetGoBase + `
// GreetAll returns a greeting for each of names.
func GreetAll(names []string) []string {
	out := make([]string, len(names))
	for i := 0; i <= len(names); i++ {
		out[i] = Greet(names[i])
	}
	return out
}
`

// normalizeLiveReviewPlanArrays mirrors internal/job/planning.go's own
// normalizePlanArrays (unexported, unreachable from here): the artifacts/
// plan.json schema declares every one of these as a bare JSON array (no
// jsonschema minItems, so a document whose XML element carries no child
// leaves the Go slice nil), and json.Marshal renders a nil slice as null,
// which the schema's "type": "array" rejects.
func normalizeLiveReviewPlanArrays(p *response.Plan) {
	if p.Design.Changes == nil {
		p.Design.Changes = []response.Change{}
	}
	if p.Design.Types == nil {
		p.Design.Types = []response.TypeDef{}
	}
	for i := range p.Design.Types {
		if p.Design.Types[i].Transitions == nil {
			p.Design.Types[i].Transitions = []response.Transition{}
		}
	}
	if p.Design.Migrations.Items == nil {
		p.Design.Migrations.Items = []response.Migration{}
	}
	if p.Delivery.Deletions.Items == nil {
		p.Delivery.Deletions.Items = []response.Fence{}
	}
}

// TestLiveReview proves a real review round (design section 6.2) finds a
// real, planted defect through the real, pinned claude CLI under the
// readonly sandbox profile (section 19.2 task 12): every lens machine.toml's
// jobs.review.lenses names runs once, at least one survivor is kept, every
// kept finding's own location falls inside the diff ROUND itself computed
// (FilterFindings, 6.3), and the worktree is byte-identical afterward --
// the readonly profile denies every write, so a clean ChangedPaths after
// the round is also a proof the profile held.
//
// This builds its own store, project, and ticket directly (InsertTicket,
// InsertArtifact) rather than through the full dispatcher
// live_build_test.go's own TestLiveBuild drives: ROUND is one handler tick,
// and this harness exists to prove that one tick against the real CLI, not
// the whole pipeline around it.
//
//	ZING_LIVE_CLI=1 go test ./cmd/zing -run TestLiveReview -v -timeout 20m
func TestLiveReview(t *testing.T) {
	if reason := liveBuildSkipReason(goruntime.GOOS, os.Getenv("ZING_LIVE_CLI")); reason != "" {
		t.Skip(reason)
	}
	oauthToken := liveClaudeOAuthToken(t)

	st := newLiveStore(t)

	dir := t.TempDir()
	if err := gitfixture.NewSigningRepo(t.Context(), dir); err != nil {
		t.Fatalf("gitfixture.NewSigningRepo: %v", err)
	}
	if err := gitfixture.AddFile(t.Context(), dir, "go.mod", []byte("module greeter\n\ngo 1.25\n")); err != nil {
		t.Fatalf("add go.mod: %v", err)
	}
	if err := gitfixture.AddFile(t.Context(), dir, "greet.go", []byte(liveReviewGreetGoBase)); err != nil {
		t.Fatalf("add greet.go: %v", err)
	}

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("load machine.toml: %v", err)
	}

	orch, err := orchestrator.New(orchestrator.Project{
		Owner: "zing-live-review", Repo: liveReviewRepoName, LocalPath: dir, DefaultBranch: liveDefaultBranch,
	}, liveReviewGitHub{}, orchestrator.NewRunner(), nil)
	if err != nil {
		t.Fatalf("build orchestrator: %v", err)
	}
	repoGit, err := orch.GitCommonDir(t.Context())
	if err != nil {
		t.Fatalf("git common dir: %v", err)
	}

	projectID, err := st.EnsureProject(t.Context(), store.Project{
		Name: liveReviewRepoName, RepoURL: "https://example.invalid/" + liveReviewRepoName, LocalPath: dir,
		Tracker: "github", DefaultBranch: liveDefaultBranch,
	})
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	const ticketTitle = "Add GreetAll"
	ticketID, err := st.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "live-review#1", Title: ticketTitle,
		Body: "Add GreetAll, a batch greeting helper.", State: "reviewing",
	})
	if err != nil {
		t.Fatalf("insert ticket: %v", err)
	}

	wt, _, err := orch.EnsureWorktree(t.Context(), ticketID, ticketTitle)
	if err != nil {
		t.Fatalf("ensure worktree: %v", err)
	}
	if plantErr := gitfixture.AddFile(t.Context(), wt.Dir(), "greet.go", []byte(liveReviewGreetGoDefect)); plantErr != nil {
		t.Fatalf("plant the defect: %v", plantErr)
	}
	sha, err := orch.HeadSHA(t.Context(), wt)
	if err != nil {
		t.Fatalf("head sha: %v", err)
	}

	claims := response.BuildClaims{FilesChanged: []string{"greet.go"}, TestExit: 0, LintExit: 0}
	report := response.BuildReport{
		TaskN:       1,
		BuildClaims: claims,
		Extras:      []response.ExtraClaim{}, Fences: []response.Fence{},
		Report: "Added GreetAll, a batch greeting helper, to greet.go.",
		Title:  ticketTitle, CommitSHA: &sha,
	}
	reportPayload, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal build report: %v", err)
	}
	if _, insertReportErr := st.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: "build_report", Payload: reportPayload}); insertReportErr != nil {
		t.Fatalf("insert build report: %v", insertReportErr)
	}

	planData, err := fixtures.FS.ReadFile("scripts/planning/2.xml")
	if err != nil {
		t.Fatalf("read plan fixture: %v", err)
	}
	doc, err := response.Parse(planData)
	if err != nil {
		t.Fatalf("parse plan fixture: %v", err)
	}
	ready, ok := doc.Response.(*response.ReadyResponse)
	if !ok {
		t.Fatalf("plan fixture response = %T, want *response.ReadyResponse", doc.Response)
	}
	plan := ready.Plan
	normalizeLiveReviewPlanArrays(&plan)
	planPayload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, insertPlanErr := st.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: "plan", Payload: planPayload}); insertPlanErr != nil {
		t.Fatalf("insert plan: %v", insertPlanErr)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a console listener: %v", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}

	dataDir := t.TempDir()
	readonlyProfile, err := zing.Assets.ReadFile("sandbox/readonly.sb")
	if err != nil {
		t.Fatalf("read embedded readonly sandbox profile: %v", err)
	}
	ro := sandbox.LoadProfile("readonly", readonlyProfile, dataDir, nil, addr.Port)
	if !ro.Available() {
		t.Fatalf("readonly sandbox did not load: %s", ro.Reason())
	}

	rts, err := runtime.NewSet(map[string]runtime.Runtime{runtimeNameClaude: runtime.NewClaude("", oauthToken)})
	if err != nil {
		t.Fatalf("build runtime set: %v", err)
	}

	owner := "live-review-owner"
	expires := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := st.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}

	deps := job.Deps{
		Store: st, Runtimes: rts, Machine: m, Models: map[string]string{"opus": "claude-opus-4-8"},
		Budget: 60 * time.Minute, Floor: response.SeverityMinor,
		Owner: owner, Expires: expires,
		Reserve: func(ctx context.Context, tID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
			return st.Reserve(ctx, tID, owner, expires, su, seed)
		},
		Sandboxes:      sandbox.Set{Build: sandbox.Off(), ReadOnly: ro, Judge: sandbox.NotLoaded()},
		RequireSandbox: true,
		Commands:       job.NewCommandRunner(sandbox.Off(), false),
		Projects:       map[int64]job.Project{projectID: {Orch: orch, RepoGit: repoGit}},
		DataDir:        dataDir, LensesParallel: 7,
	}

	ticket, err := st.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("get ticket: %v", err)
	}

	commit, err := job.Registry()["reviewing"].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("review round: %v", err)
	}
	if validateErr := job.ValidateCommit(ticket, commit); validateErr != nil {
		t.Fatalf("validate commit: %v", validateErr)
	}
	applied, err := st.CommitHandlerResult(t.Context(), commit)
	if err != nil || !applied {
		t.Fatalf("commit handler result: applied=%v err=%v", applied, err)
	}

	runs, err := st.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("runs for ticket: %v", err)
	}
	lensRuns := 0
	for _, r := range runs {
		if r.Lens != nil {
			lensRuns++
		}
	}
	wantLenses := len(m.Jobs["review"].Lenses)
	if lensRuns != wantLenses {
		t.Errorf("lens runs = %d, want %d (one per machine.toml jobs.review.lenses)", lensRuns, wantLenses)
	}

	findings, err := st.Findings(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("findings: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("findings = none, want at least one kept (the planted off-by-one)")
	}

	diff, err := orch.Diff(t.Context(), wt, sha)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	idx := orchestrator.ParseDiff(diff)
	for _, f := range findings {
		path, line, parseOK := job.ParseLocation(f.Finding.Location)
		if !parseOK {
			t.Errorf("finding %s: location %q does not parse", f.Finding.ID, f.Finding.Location)
			continue
		}
		if !idx.Contains(path, line) {
			t.Errorf("finding %s: location %s:%d is outside the diff", f.Finding.ID, path, line)
		}
	}

	changed, err := orch.ChangedPaths(t.Context(), wt)
	if err != nil {
		t.Fatalf("changed paths: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed paths after the round = %v, want none (the readonly profile writes nothing, and ROUND itself writes only to the store)", changed)
	}
}
