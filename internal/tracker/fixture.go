package tracker

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// Fixture is a Tracker backed by a repo-owned TOML file, read once at
// construction and served unchanged on every call. It carries no
// connection, so it never fails after construction.
type Fixture struct {
	mu       sync.Mutex
	project  string
	tickets  []Ticket
	nextRef  int
	comments map[string][]string // ref -> every Comment body posted, in post order
	closed   map[string]bool     // ref -> whether Close has been called on it
}

// Guarantee the concrete type satisfies the interface, the same proof
// github.go's own GitHubTracker carries.
var _ Tracker = (*Fixture)(nil)

// fixtureDoc is fixtures/tickets.toml's shape: one project name and its
// tickets.
type fixtureDoc struct {
	Project string         `toml:"project"`
	Ticket  []fixtureEntry `toml:"ticket"`
}

type fixtureEntry struct {
	Ref   string `toml:"ref"`
	Title string `toml:"title"`
	Body  string `toml:"body"`
}

// NewFixture reads path out of fsys once and returns a Fixture serving its
// tickets. path is TOML in the fixtureDoc shape.
func NewFixture(fsys fs.FS, path string) (*Fixture, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, fmt.Errorf("tracker: read fixture %q: %w", path, err)
	}

	var doc fixtureDoc
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, fmt.Errorf("tracker: decode fixture %q: %w", path, err)
	}

	tickets := make([]Ticket, 0, len(doc.Ticket))
	for _, e := range doc.Ticket {
		tickets = append(tickets, Ticket(e))
	}

	return &Fixture{
		project:  doc.Project,
		tickets:  tickets,
		nextRef:  maxNumericRef(tickets) + 1,
		comments: make(map[string][]string),
		closed:   make(map[string]bool),
	}, nil
}

// maxNumericRef returns the largest trailing numeric suffix among tickets'
// Ref fields (for example "fake#7" -> 7), or 0 if none carries one. NewFixture
// seeds nextRef from this, one past the highest ref already loaded, rather
// than len(tickets)+1: a loaded ticket set is not guaranteed sequential (a
// gap, or a ref out of "fake#<n>" order), so counting entries instead of
// parsing the refs themselves could mint a "fake#<n>" that collides with one
// already loaded.
func maxNumericRef(tickets []Ticket) int {
	highest := 0
	for _, t := range tickets {
		if n, ok := numericSuffix(t.Ref); ok && n > highest {
			highest = n
		}
	}
	return highest
}

// numericSuffix parses the trailing run of ASCII digits in ref (for example
// "fake#7" -> 7), reporting ok=false when ref carries no trailing digit at
// all.
func numericSuffix(ref string) (n int, ok bool) {
	i := len(ref)
	for i > 0 && ref[i-1] >= '0' && ref[i-1] <= '9' {
		i--
	}
	if i == len(ref) {
		return 0, false
	}
	v, err := strconv.Atoi(ref[i:])
	if err != nil {
		return 0, false
	}
	return v, true
}

// Intake returns every fixture ticket when project matches the fixture's
// own project name, or none otherwise. rule is unused: the fixture format
// carries no per-ticket assignee to filter on.
func (f *Fixture) Intake(_ context.Context, project string, _ IntakeRule) ([]Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if project != f.project {
		return nil, nil
	}
	return append([]Ticket(nil), f.tickets...), nil
}

// Fetch returns the ticket matching ref within project.
func (f *Fixture) Fetch(_ context.Context, project, ref string) (Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if project != f.project {
		return Ticket{}, fmt.Errorf("tracker: unknown project %q", project)
	}
	for _, t := range f.tickets {
		if t.Ref == ref {
			return t, nil
		}
	}
	return Ticket{}, fmt.Errorf("tracker: no ticket %q in project %q", ref, project)
}

// Issue returns the ticket matching ref within project, the same lookup
// Fetch makes, but with ErrIssueNotFound (PKG9-PLAN.md D29) in place of
// Fetch's own generic "no ticket" error when ref is unknown: the fixture
// carries no closed or pull-request state to test ErrIssueClosed or
// ErrIssueIsPullRequest against (those are exercised against the real
// GitHub tracker, github_test.go).
func (f *Fixture) Issue(_ context.Context, project, ref string) (Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if project != f.project {
		return Ticket{}, fmt.Errorf("tracker: unknown project %q", project)
	}
	for _, t := range f.tickets {
		if t.Ref == ref {
			return t, nil
		}
	}
	return Ticket{}, ErrIssueNotFound
}

// Comment records a structured log line for body against ref, keeps body
// in memory under ref for CommentContains to search, and returns nil. The
// log line carries body's length, never body itself: a comment body can
// carry sensitive text, and the repo rule is never log a secret.
func (f *Fixture) Comment(_ context.Context, project, ref, body string) error {
	slog.Info("tracker fixture comment", "project", project, "ref", ref, "body_len", len(body))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[ref] = append(f.comments[ref], body)
	return nil
}

// CommentContains reports whether ref carries a recorded comment
// containing needle (design section 10.5). The fixture posts every comment
// under its own single identity, so no other-author case can arise the way
// it can against a real tracker.
func (f *Fixture) CommentContains(_ context.Context, project, ref, needle string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if project != f.project {
		return false, fmt.Errorf("tracker: unknown project %q", project)
	}
	for _, body := range f.comments[ref] {
		if strings.Contains(body, needle) {
			return true, nil
		}
	}
	return false, nil
}

// Close records ref as closed (design section 10.5, PKG9-PLAN.md section
// 8.6): closing an already-closed ref is a no-op, matching the real
// tracker's own idempotent behavior.
func (f *Fixture) Close(_ context.Context, project, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if project != f.project {
		return fmt.Errorf("tracker: unknown project %q", project)
	}
	f.closed[ref] = true
	return nil
}

// Closed reports whether Close has been called on ref, so a test can
// assert Close's effect without a real tracker to read it back from.
func (f *Fixture) Closed(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed[ref]
}

// FileTicket appends t to the in-memory ticket list under a generated
// fake#<n> ref and returns it.
func (f *Fixture) FileTicket(_ context.Context, project string, t NewTicket) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if project != f.project {
		return "", fmt.Errorf("tracker: unknown project %q", project)
	}

	ref := fmt.Sprintf("fake#%d", f.nextRef)
	f.nextRef++
	f.tickets = append(f.tickets, Ticket{Ref: ref, Title: t.Title, Body: t.Body})
	return ref, nil
}

// Collaborators returns a fixed single-name slice; the fixture has no real
// collaborator source to read.
func (f *Fixture) Collaborators(_ context.Context, project string) ([]string, error) {
	if project != f.project {
		return nil, fmt.Errorf("tracker: unknown project %q", project)
	}
	return []string{"fixture-user"}, nil
}
