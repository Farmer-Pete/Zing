package tracker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"
)

// testGHToken is the token every newTestTracker build authenticates with, so
// a test can assert the Authorization header it produces.
const testGHToken = "test-token-123" //nolint:gosec // not a credential, a fixed test fixture value

// Fixture values shared across the tests below, factored out because
// goconst flags a literal repeated three or more times.
const (
	testProject  = "proj"  // the project name every non-construction test maps to testRepoSpec
	testRepoSpec = "o/r"   // that project's "owner/name" repo string
	testAssignee = "alice" // the assignee name used in Intake's happy-path and gate tests
	testTitle    = "New"   // the issue title used in FileTicket's tests
)

// newTestTracker builds a *GitHubTracker whose *github.Client points at an
// httptest server backed by mux, mirroring
// internal/orchestrator/github_test.go's newTestGHClient. It builds the
// concrete struct directly, bypassing NewGitHub's client construction (which
// has no WithURLs hook), but reuses parseRepo so the repos map is built the
// same way NewGitHub builds it.
func newTestTracker(t *testing.T, mux *http.ServeMux, repos map[string]string) *GitHubTracker {
	t.Helper()

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	serverURL := server.URL + "/"
	c, err := github.NewClient(github.WithAuthToken(testGHToken), github.WithURLs(&serverURL, &serverURL))
	if err != nil {
		t.Fatalf("github.NewClient: %v", err)
	}

	parsed := make(map[string]repo, len(repos))
	for project, s := range repos {
		r, err := parseRepo(s)
		if err != nil {
			t.Fatalf("parseRepo(%q): %v", s, err)
		}
		parsed[project] = r
	}

	return &GitHubTracker{c: c, repos: parsed}
}

// newDefaultTracker is newTestTracker with the one-entry repo map every
// non-construction test uses: project testProject mapping to testRepoSpec.
func newDefaultTracker(t *testing.T, mux *http.ServeMux) *GitHubTracker {
	t.Helper()
	return newTestTracker(t, mux, map[string]string{testProject: testRepoSpec})
}

// unhitMux is a ServeMux that fails the test if any handler is ever reached,
// for asserting a gate rejects before any HTTP call.
func unhitMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected HTTP request: %s %s", r.Method, r.URL.String())
	})
	return mux
}

func TestNewGitHub(t *testing.T) {
	t.Run("empty token is an error", func(t *testing.T) {
		_, err := NewGitHub("", map[string]string{testProject: testRepoSpec})
		if err == nil {
			t.Fatal("NewGitHub: expected an error for an empty token, got nil")
		}
		if !strings.Contains(err.Error(), "token must not be empty") {
			t.Errorf("NewGitHub error = %q, want it to mention %q", err.Error(), "token must not be empty")
		}
	})

	t.Run("empty repos map is an error", func(t *testing.T) {
		_, err := NewGitHub("tok", nil)
		if err == nil {
			t.Fatal("NewGitHub: expected an error for an empty repos map, got nil")
		}
	})

	t.Run("a repo value with no slash is an error naming the project", func(t *testing.T) {
		_, err := NewGitHub("tok", map[string]string{testProject: "no-slash-here"})
		if err == nil {
			t.Fatal("NewGitHub: expected an error, got nil")
		}
		if !strings.Contains(err.Error(), testProject) {
			t.Errorf("NewGitHub error = %q, want it to name the offending project %q", err.Error(), testProject)
		}
	})

	t.Run("a repo value with an empty half is an error naming the project", func(t *testing.T) {
		_, err := NewGitHub("tok", map[string]string{testProject: "owner/"})
		if err == nil {
			t.Fatal("NewGitHub: expected an error, got nil")
		}
		if !strings.Contains(err.Error(), testProject) {
			t.Errorf("NewGitHub error = %q, want it to name the offending project %q", err.Error(), testProject)
		}
	})

	t.Run("a valid map succeeds and returns the concrete type", func(t *testing.T) {
		gh, err := NewGitHub("tok", map[string]string{testProject: testRepoSpec})
		if err != nil {
			t.Fatalf("NewGitHub: unexpected error: %v", err)
		}
		if gh == nil {
			t.Fatal("NewGitHub: expected a non-nil *GitHubTracker")
		}
	})
}

func TestParseRepo(t *testing.T) {
	tests := []struct {
		in      string
		want    repo
		wantErr bool
	}{
		{in: "owner/name", want: repo{owner: "owner", name: "name"}},
		{in: "noslash", wantErr: true},
		{in: "owner/", wantErr: true},
		{in: "/name", wantErr: true},
		{in: "owner/name/extra", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseRepo(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseRepo(%q): expected an error, got nil", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRepo(%q): unexpected error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseRepo(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestGitHubTrackerIntake(t *testing.T) {
	t.Run("unions two pages, drops a PR, keeps an assigned zing:proposed issue", func(t *testing.T) {
		var gotAssignee, gotState, gotPage1, gotPage2 string
		var hits int

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
			hits++
			page := r.URL.Query().Get("page")
			if page == "2" {
				gotPage2 = page
				fmt.Fprint(w, `[{"number":3,"title":"Assigned proposed","body":"b3"}]`)
				return
			}
			gotPage1 = "1"
			gotAssignee = r.URL.Query().Get("assignee")
			gotState = r.URL.Query().Get("state")
			w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/o/r/issues?page=2>; rel="next"`)
			fmt.Fprint(w, `[
				{"number":1,"title":"A pull request","body":"b1","pull_request":{"url":"x"}},
				{"number":2,"title":"An issue","body":"b2"}
			]`)
		})

		g := newDefaultTracker(t, mux)

		tickets, err := g.Intake(t.Context(), testProject, IntakeRule{Assignee: testAssignee})
		if err != nil {
			t.Fatalf("Intake: unexpected error: %v", err)
		}
		if hits != 2 {
			t.Fatalf("Intake: made %d requests, want 2 (two pages)", hits)
		}
		if gotPage1 != "1" {
			t.Error("Intake: first page request was never observed")
		}
		if gotPage2 != "2" {
			t.Errorf("Intake: second-page request page query = %q, want %q", gotPage2, "2")
		}
		if gotAssignee != testAssignee {
			t.Errorf("Intake: assignee query = %q, want %q", gotAssignee, testAssignee)
		}
		if gotState != "open" {
			t.Errorf("Intake: state query = %q, want %q", gotState, "open")
		}

		want := map[string]string{"2": "An issue", "3": "Assigned proposed"}
		if len(tickets) != len(want) {
			t.Fatalf("Intake tickets = %+v, want %d tickets %v", tickets, len(want), want)
		}
		for _, tk := range tickets {
			if want[tk.Ref] != tk.Title {
				t.Errorf("Intake: unexpected ticket %+v", tk)
			}
			if tk.Ref == "1" {
				t.Error("Intake: the pull request (issue #1) leaked into the tickets")
			}
		}
	})

	t.Run("rejects an empty assignee before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		_, err := g.Intake(t.Context(), testProject, IntakeRule{Assignee: ""})
		if err == nil {
			t.Fatal("Intake: expected an error for an empty assignee, got nil")
		}
	})

	t.Run(`rejects assignee "none" before any HTTP call`, func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		_, err := g.Intake(t.Context(), testProject, IntakeRule{Assignee: "none"})
		if err == nil {
			t.Fatal(`Intake: expected an error for assignee "none", got nil`)
		}
	})

	t.Run(`rejects assignee "*" before any HTTP call`, func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		_, err := g.Intake(t.Context(), testProject, IntakeRule{Assignee: "*"})
		if err == nil {
			t.Fatal(`Intake: expected an error for assignee "*", got nil`)
		}
	})

	t.Run("a first-page 500 is an error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		g := newDefaultTracker(t, mux)

		_, err := g.Intake(t.Context(), testProject, IntakeRule{Assignee: testAssignee})
		if err == nil {
			t.Fatal("Intake: expected an error for a first-page 500, got nil")
		}
		if !strings.Contains(err.Error(), "tracker: intake:") {
			t.Errorf("Intake error = %q, want it to carry the %q prefix", err.Error(), "tracker: intake:")
		}
	})

	t.Run("a second-page 500 errors and leaks no partial slice", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/o/r/issues?page=2>; rel="next"`)
			fmt.Fprint(w, `[{"number":1,"title":"An issue","body":"b1"}]`)
		})
		g := newDefaultTracker(t, mux)

		tickets, err := g.Intake(t.Context(), testProject, IntakeRule{Assignee: testAssignee})
		if err == nil {
			t.Fatal("Intake: expected an error for a second-page 500, got nil")
		}
		if tickets != nil {
			t.Errorf("Intake: tickets = %+v, want nil on a second-page failure (no partial leak)", tickets)
		}
	})

	t.Run("unknown project errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		_, err := g.Intake(t.Context(), "no-such-project", IntakeRule{Assignee: testAssignee})
		if err == nil {
			t.Fatal("Intake: expected an error for an unknown project, got nil")
		}
		if !strings.Contains(err.Error(), "unknown project") {
			t.Errorf("Intake error = %q, want it to mention %q", err.Error(), "unknown project")
		}
	})
}

func TestGitHubTrackerFetch(t *testing.T) {
	t.Run("returns the ticket", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"number":42,"title":"A title","body":"A body"}`)
		})
		g := newDefaultTracker(t, mux)

		tk, err := g.Fetch(t.Context(), testProject, "42")
		if err != nil {
			t.Fatalf("Fetch: unexpected error: %v", err)
		}
		want := Ticket{Ref: "42", Title: "A title", Body: "A body"}
		if tk != want {
			t.Errorf("Fetch = %+v, want %+v", tk, want)
		}
	})

	t.Run("a non-canonical ref errors before any HTTP call", func(t *testing.T) {
		for _, ref := range []string{"+1", "0", "-3", "007", " 7 ", "abc", ""} {
			t.Run(ref, func(t *testing.T) {
				g := newDefaultTracker(t, unhitMux(t))
				_, err := g.Fetch(t.Context(), testProject, ref)
				if err == nil {
					t.Fatalf("Fetch(%q): expected an error, got nil", ref)
				}
			})
		}
	})

	t.Run("a 404 is an error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		g := newDefaultTracker(t, mux)

		_, err := g.Fetch(t.Context(), testProject, "42")
		if err == nil {
			t.Fatal("Fetch: expected an error for a 404, got nil")
		}
		if !strings.Contains(err.Error(), "tracker: fetch:") {
			t.Errorf("Fetch error = %q, want it to carry the %q prefix", err.Error(), "tracker: fetch:")
		}
	})

	t.Run("unknown project errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		_, err := g.Fetch(t.Context(), "no-such-project", "42")
		if err == nil {
			t.Fatal("Fetch: expected an error for an unknown project, got nil")
		}
		if !strings.Contains(err.Error(), "unknown project") {
			t.Errorf("Fetch error = %q, want it to mention %q", err.Error(), "unknown project")
		}
	})
}

func TestGitHubTrackerComment(t *testing.T) {
	t.Run("posts the body to the right path with the auth header", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth string
		var gotBody map[string]any

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode request body: %v", err)
				return
			}
			fmt.Fprint(w, `{"id":1}`)
		})
		g := newDefaultTracker(t, mux)

		err := g.Comment(t.Context(), testProject, "42", "hello there")
		if err != nil {
			t.Fatalf("Comment: unexpected error: %v", err)
		}
		if gotMethod != http.MethodPost {
			t.Errorf("Comment: method = %q, want %q", gotMethod, http.MethodPost)
		}
		if gotPath != "/repos/o/r/issues/42/comments" {
			t.Errorf("Comment: path = %q, want %q", gotPath, "/repos/o/r/issues/42/comments")
		}
		if want := "Bearer " + testGHToken; gotAuth != want {
			t.Errorf("Comment: Authorization header = %q, want %q", gotAuth, want)
		}
		if gotBody["body"] != "hello there" {
			t.Errorf("Comment: request body = %v, want body %q", gotBody, "hello there")
		}
	})

	t.Run("a non-canonical ref errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		if err := g.Comment(t.Context(), testProject, "+1", "hi"); err == nil {
			t.Fatal("Comment: expected an error for a non-canonical ref, got nil")
		}
	})

	t.Run("a 500 is an error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42/comments", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		g := newDefaultTracker(t, mux)

		err := g.Comment(t.Context(), testProject, "42", "hi")
		if err == nil {
			t.Fatal("Comment: expected an error for a 500, got nil")
		}
		if !strings.Contains(err.Error(), "tracker: comment:") {
			t.Errorf("Comment error = %q, want it to carry the %q prefix", err.Error(), "tracker: comment:")
		}
	})

	t.Run("unknown project errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		if err := g.Comment(t.Context(), "no-such-project", "42", "hi"); err == nil {
			t.Fatal("Comment: expected an error for an unknown project, got nil")
		}
	})
}

func TestGitHubTrackerFileTicket(t *testing.T) {
	t.Run("creates an unassigned zing:proposed issue and returns the decimal ref", func(t *testing.T) {
		var gotBody map[string]any

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode request body: %v", err)
				return
			}
			fmt.Fprint(w, `{"number":99}`)
		})
		g := newDefaultTracker(t, mux)

		ref, err := g.FileTicket(t.Context(), testProject, NewTicket{Title: testTitle, Body: "Body text"})
		if err != nil {
			t.Fatalf("FileTicket: unexpected error: %v", err)
		}
		if ref != "99" {
			t.Errorf("FileTicket ref = %q, want %q", ref, "99")
		}
		labels, ok := gotBody["labels"].([]any)
		if !ok || len(labels) != 1 || labels[0] != proposedLabel {
			t.Errorf("FileTicket: request body labels = %v, want [%q]", gotBody["labels"], proposedLabel)
		}
		if _, present := gotBody["assignee"]; present {
			t.Error(`FileTicket: request body carries "assignee", want it absent`)
		}
		if _, present := gotBody["assignees"]; present {
			t.Error(`FileTicket: request body carries "assignees", want it absent`)
		}
	})

	t.Run("appends a depends-on line when DependsOn is set", func(t *testing.T) {
		var gotBody map[string]any

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode request body: %v", err)
				return
			}
			fmt.Fprint(w, `{"number":100}`)
		})
		g := newDefaultTracker(t, mux)

		_, err := g.FileTicket(t.Context(), testProject, NewTicket{
			Title:     testTitle,
			Body:      "Body text",
			DependsOn: []string{"5", "#6"},
		})
		if err != nil {
			t.Fatalf("FileTicket: unexpected error: %v", err)
		}
		want := "Body text\n\nDepends on #5, #6"
		if gotBody["body"] != want {
			t.Errorf("FileTicket: request body body = %q, want %q", gotBody["body"], want)
		}
	})

	t.Run("a 500 is an error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		g := newDefaultTracker(t, mux)

		_, err := g.FileTicket(t.Context(), testProject, NewTicket{Title: testTitle, Body: "Body"})
		if err == nil {
			t.Fatal("FileTicket: expected an error for a 500, got nil")
		}
		if !strings.Contains(err.Error(), "tracker: file ticket:") {
			t.Errorf("FileTicket error = %q, want it to carry the %q prefix", err.Error(), "tracker: file ticket:")
		}
	})

	t.Run("unknown project errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		_, err := g.FileTicket(t.Context(), "no-such-project", NewTicket{Title: testTitle, Body: "Body"})
		if err == nil {
			t.Fatal("FileTicket: expected an error for an unknown project, got nil")
		}
	})
}

func TestGitHubTrackerCollaborators(t *testing.T) {
	t.Run("unions two pages and skips an empty login", func(t *testing.T) {
		var hits int
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/collaborators", func(w http.ResponseWriter, r *http.Request) {
			hits++
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `[{"login":"dave"},{"login":""}]`)
				return
			}
			w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/o/r/collaborators?page=2>; rel="next"`)
			fmt.Fprint(w, `[{"login":"carol"}]`)
		})
		g := newDefaultTracker(t, mux)

		logins, err := g.Collaborators(t.Context(), testProject)
		if err != nil {
			t.Fatalf("Collaborators: unexpected error: %v", err)
		}
		if hits != 2 {
			t.Fatalf("Collaborators: made %d requests, want 2", hits)
		}
		want := map[string]bool{"carol": true, "dave": true}
		if len(logins) != len(want) {
			t.Fatalf("Collaborators = %v, want %d logins %v", logins, len(want), want)
		}
		for _, l := range logins {
			if !want[l] {
				t.Errorf("Collaborators contains unexpected login %q", l)
			}
		}
	})

	t.Run("a 500 is an error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/collaborators", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		g := newDefaultTracker(t, mux)

		_, err := g.Collaborators(t.Context(), testProject)
		if err == nil {
			t.Fatal("Collaborators: expected an error for a 500, got nil")
		}
		if !strings.Contains(err.Error(), "tracker: collaborators:") {
			t.Errorf("Collaborators error = %q, want it to carry the %q prefix", err.Error(), "tracker: collaborators:")
		}
	})

	t.Run("unknown project errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		_, err := g.Collaborators(t.Context(), "no-such-project")
		if err == nil {
			t.Fatal("Collaborators: expected an error for an unknown project, got nil")
		}
		if !strings.Contains(err.Error(), "unknown project") {
			t.Errorf("Collaborators error = %q, want it to mention %q", err.Error(), "unknown project")
		}
	})
}
