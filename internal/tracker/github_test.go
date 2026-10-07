package tracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"
)

// assertErrorsAsGitHubErrorResponse fails the test unless errors.As finds a
// *github.ErrorResponse in err's chain: go-github wraps every HTTP failure
// in that type, and every tracker method wraps it again with fmt.Errorf's
// %w, so a caller that needs the underlying HTTP detail (status code,
// message) must be able to unwrap to it. Asserting on the error string
// alone would still pass if a future change swapped %w for %v and broke
// that chain silently.
func assertErrorsAsGitHubErrorResponse(t *testing.T, err error) {
	t.Helper()
	ghErr, ok := errors.AsType[*github.ErrorResponse](err)
	if !ok {
		t.Errorf("errors.AsType[*github.ErrorResponse](%v) found no match, want one", err)
		return
	}
	if ghErr.Response == nil {
		t.Error("errors.AsType found a *github.ErrorResponse, but its Response is nil")
	}
}

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

	t.Run("an empty project key is an error", func(t *testing.T) {
		_, err := NewGitHub("tok", map[string]string{"": testRepoSpec})
		if err == nil {
			t.Fatal("NewGitHub: expected an error for an empty project key, got nil")
		}
		if !strings.Contains(err.Error(), errEmptyProjectName.Error()) {
			t.Errorf("NewGitHub error = %q, want it to mention %q", err.Error(), errEmptyProjectName.Error())
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
		gotRefs := make(map[string]string, len(tickets)) // ref -> title, so a missing page-two ref cannot be masked by a duplicate page-one one
		for _, tk := range tickets {
			gotRefs[tk.Ref] = tk.Title
			if tk.Ref == "1" {
				t.Error("Intake: the pull request (issue #1) leaked into the tickets")
			}
		}
		if len(tickets) != len(gotRefs) {
			t.Errorf("Intake tickets = %+v, want no duplicate refs", tickets)
		}
		if !maps.Equal(gotRefs, want) {
			t.Errorf("Intake tickets (by ref) = %v, want exactly %v", gotRefs, want)
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
		assertErrorsAsGitHubErrorResponse(t, err)
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
		assertErrorsAsGitHubErrorResponse(t, err)
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
		assertErrorsAsGitHubErrorResponse(t, err)
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

// TestGitHubTrackerIssue proves Issue (PKG9-PLAN.md D29) returns the open
// issue, and maps a 404, a closed issue, and a pull request each to their
// own typed sentinel.
func TestGitHubTrackerIssue(t *testing.T) {
	t.Run("returns the open issue", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"number":42,"title":"A title","body":"A body","state":"open"}`)
		})
		g := newDefaultTracker(t, mux)

		tk, err := g.Issue(t.Context(), testProject, "42")
		if err != nil {
			t.Fatalf("Issue: unexpected error: %v", err)
		}
		want := Ticket{Ref: "42", Title: "A title", Body: "A body"}
		if tk != want {
			t.Errorf("Issue = %+v, want %+v", tk, want)
		}
	})

	t.Run("a 404 becomes ErrIssueNotFound", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		g := newDefaultTracker(t, mux)

		_, err := g.Issue(t.Context(), testProject, "42")
		if !errors.Is(err, ErrIssueNotFound) {
			t.Errorf("Issue err = %v, want errors.Is ErrIssueNotFound", err)
		}
	})

	t.Run("a closed issue becomes ErrIssueClosed", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"number":42,"title":"A title","body":"A body","state":"closed"}`)
		})
		g := newDefaultTracker(t, mux)

		_, err := g.Issue(t.Context(), testProject, "42")
		if !errors.Is(err, ErrIssueClosed) {
			t.Errorf("Issue err = %v, want errors.Is ErrIssueClosed", err)
		}
	})

	t.Run("a pull request becomes ErrIssueIsPullRequest, even when also marked closed", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"number":42,"title":"A title","body":"A body","state":"closed","pull_request":{"url":"https://api.github.com/repos/o/r/pulls/42"}}`)
		})
		g := newDefaultTracker(t, mux)

		_, err := g.Issue(t.Context(), testProject, "42")
		if !errors.Is(err, ErrIssueIsPullRequest) {
			t.Errorf("Issue err = %v, want errors.Is ErrIssueIsPullRequest (checked before the closed state)", err)
		}
	})

	t.Run("a 500 is a wrapped error, not a sentinel", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		g := newDefaultTracker(t, mux)

		_, err := g.Issue(t.Context(), testProject, "42")
		if err == nil {
			t.Fatal("Issue: expected an error for a 500, got nil")
		}
		if errors.Is(err, ErrIssueNotFound) || errors.Is(err, ErrIssueClosed) || errors.Is(err, ErrIssueIsPullRequest) {
			t.Errorf("Issue err = %v, want none of the three sentinels for a 500", err)
		}
		if !strings.Contains(err.Error(), "tracker: issue:") {
			t.Errorf("Issue error = %q, want it to carry the %q prefix", err.Error(), "tracker: issue:")
		}
		assertErrorsAsGitHubErrorResponse(t, err)
	})

	t.Run("a non-canonical ref errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		if _, err := g.Issue(t.Context(), testProject, "+1"); err == nil {
			t.Fatal("Issue: expected an error for a non-canonical ref, got nil")
		}
	})

	t.Run("unknown project errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		_, err := g.Issue(t.Context(), "no-such-project", "42")
		if err == nil {
			t.Fatal("Issue: expected an error for an unknown project, got nil")
		}
		if !strings.Contains(err.Error(), "unknown project") {
			t.Errorf("Issue error = %q, want it to mention %q", err.Error(), "unknown project")
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
		assertErrorsAsGitHubErrorResponse(t, err)
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
		assertErrorsAsGitHubErrorResponse(t, err)
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
		gotLogins := make(map[string]bool, len(logins)) // set, so a missing page-two login cannot be masked by a duplicate page-one one
		for _, l := range logins {
			gotLogins[l] = true
		}
		if len(logins) != len(gotLogins) {
			t.Errorf("Collaborators = %v, want no duplicate logins", logins)
		}
		if !maps.Equal(gotLogins, want) {
			t.Errorf("Collaborators (as a set) = %v, want exactly %v", gotLogins, want)
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
		assertErrorsAsGitHubErrorResponse(t, err)
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

func TestCloseIssue(t *testing.T) {
	t.Run("sends state closed and reason completed", func(t *testing.T) {
		var gotMethod, gotPath string
		var gotBody map[string]any

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42", func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode request body: %v", err)
				return
			}
			fmt.Fprint(w, `{"number":42,"state":"closed"}`)
		})
		g := newDefaultTracker(t, mux)

		if err := g.Close(t.Context(), testProject, "42"); err != nil {
			t.Fatalf("Close: unexpected error: %v", err)
		}
		if gotMethod != http.MethodPatch {
			t.Errorf("Close: method = %q, want %q", gotMethod, http.MethodPatch)
		}
		if gotPath != "/repos/o/r/issues/42" {
			t.Errorf("Close: path = %q, want %q", gotPath, "/repos/o/r/issues/42")
		}
		if gotBody["state"] != "closed" {
			t.Errorf("Close: request state = %v, want %q", gotBody["state"], "closed")
		}
		if gotBody["state_reason"] != "completed" {
			t.Errorf("Close: request state_reason = %v, want %q", gotBody["state_reason"], "completed")
		}
	})

	t.Run("an already-closed issue still succeeds", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"number":42,"state":"closed"}`)
		})
		g := newDefaultTracker(t, mux)

		if err := g.Close(t.Context(), testProject, "42"); err != nil {
			t.Fatalf("Close on an already-closed issue: unexpected error: %v", err)
		}
	})

	t.Run("a 500 is an error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		g := newDefaultTracker(t, mux)

		err := g.Close(t.Context(), testProject, "42")
		if err == nil {
			t.Fatal("Close: expected an error for a 500, got nil")
		}
		if !strings.Contains(err.Error(), "tracker: close:") {
			t.Errorf("Close error = %q, want it to carry the %q prefix", err.Error(), "tracker: close:")
		}
		assertErrorsAsGitHubErrorResponse(t, err)
	})

	t.Run("unknown project errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		if err := g.Close(t.Context(), "no-such-project", "42"); err == nil {
			t.Fatal("Close: expected an error for an unknown project, got nil")
		}
	})

	t.Run("a non-canonical ref errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		if err := g.Close(t.Context(), testProject, "+1"); err == nil {
			t.Fatal("Close: expected an error for a non-canonical ref, got nil")
		}
	})
}

// testViewerLoginGH is the authenticated login Users.Get reports across
// this file's CommentContains tests, named once so goconst has nothing to
// flag.
const testViewerLoginGH = "zing-bot"

func TestCommentContainsPagesAll(t *testing.T) {
	const needle = "<!-- zing:pr t9 -->"

	t.Run("finds a needle on the third page", func(t *testing.T) {
		var hits int

		mux := http.NewServeMux()
		mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"login":%q}`, testViewerLoginGH)
		})
		mux.HandleFunc("/repos/o/r/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
			hits++
			switch r.URL.Query().Get("page") {
			case "", "1":
				w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/o/r/issues/42/comments?page=2>; rel="next"`)
				fmt.Fprintf(w, `[{"user":{"login":%q},"body":"unrelated 1"}]`, testViewerLoginGH)
			case "2":
				w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/o/r/issues/42/comments?page=3>; rel="next"`)
				fmt.Fprintf(w, `[{"user":{"login":%q},"body":"unrelated 2"}]`, testViewerLoginGH)
			case "3":
				fmt.Fprintf(w, `[{"user":{"login":%q},"body":"the link is here: %s"}]`, testViewerLoginGH, needle)
			default:
				t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			}
		})
		g := newDefaultTracker(t, mux)

		found, err := g.CommentContains(t.Context(), testProject, "42", needle)
		if err != nil {
			t.Fatalf("CommentContains: unexpected error: %v", err)
		}
		if !found {
			t.Error("CommentContains = false, want true (the needle is on page 3)")
		}
		if hits != 3 {
			t.Errorf("CommentContains made %d requests, want 3 (all three pages read)", hits)
		}
	})

	t.Run("a needle nowhere is not found, after reading every page", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"login":%q}`, testViewerLoginGH)
		})
		mux.HandleFunc("/repos/o/r/issues/42/comments", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `[{"user":{"login":%q},"body":"nothing to see here"}]`, testViewerLoginGH)
		})
		g := newDefaultTracker(t, mux)

		found, err := g.CommentContains(t.Context(), testProject, "42", needle)
		if err != nil {
			t.Fatalf("CommentContains: unexpected error: %v", err)
		}
		if found {
			t.Error("CommentContains = true, want false")
		}
	})

	t.Run("a 500 on Users.Get is an error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		g := newDefaultTracker(t, mux)

		if _, err := g.CommentContains(t.Context(), testProject, "42", needle); err == nil {
			t.Fatal("CommentContains: expected an error when Users.Get fails, got nil")
		}
	})

	t.Run("unknown project errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		if _, err := g.CommentContains(t.Context(), "no-such-project", "42", needle); err == nil {
			t.Fatal("CommentContains: expected an error for an unknown project, got nil")
		}
	})
}

func TestGitHubTrackerCommentsPagesAll(t *testing.T) {
	t.Run("returns 101 comments across two pages, in order", func(t *testing.T) {
		var hits int

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
			hits++
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `[{"id":101,"user":{"login":"alice"},"body":"last one"}]`)
				return
			}
			w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/o/r/issues/42/comments?page=2>; rel="next"`)
			var body strings.Builder
			body.WriteString("[")
			for i := 1; i <= 100; i++ {
				if i > 1 {
					body.WriteString(",")
				}
				fmt.Fprintf(&body, `{"id":%d,"user":{"login":"alice"},"body":"comment %d"}`, i, i)
			}
			body.WriteString("]")
			fmt.Fprint(w, body.String())
		})
		g := newDefaultTracker(t, mux)

		cs, err := g.Comments(t.Context(), testProject, "42")
		if err != nil {
			t.Fatalf("Comments: unexpected error: %v", err)
		}
		if hits != 2 {
			t.Fatalf("Comments: made %d requests, want 2 (two pages)", hits)
		}
		if len(cs) != 101 {
			t.Fatalf("Comments returned %d entries, want 101", len(cs))
		}
		if cs[0].ID != 1 || cs[0].Author != "alice" || cs[0].Body != "comment 1" {
			t.Errorf("Comments[0] = %+v, want {ID:1 Author:alice Body:\"comment 1\"}", cs[0])
		}
		if cs[100].ID != 101 || cs[100].Body != "last one" {
			t.Errorf("Comments[100] = %+v, want the last page's one entry", cs[100])
		}
	})

	t.Run("a non-canonical ref errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		if _, err := g.Comments(t.Context(), testProject, "+1"); err == nil {
			t.Fatal("Comments: expected an error for a non-canonical ref, got nil")
		}
	})

	t.Run("unknown project errors before any HTTP call", func(t *testing.T) {
		g := newDefaultTracker(t, unhitMux(t))
		if _, err := g.Comments(t.Context(), "no-such-project", "42"); err == nil {
			t.Fatal("Comments: expected an error for an unknown project, got nil")
		}
	})

	t.Run("a 500 is an error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/r/issues/42/comments", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		g := newDefaultTracker(t, mux)

		_, err := g.Comments(t.Context(), testProject, "42")
		if err == nil {
			t.Fatal("Comments: expected an error for a 500, got nil")
		}
		if !strings.Contains(err.Error(), "tracker: comments:") {
			t.Errorf("Comments error = %q, want it to carry the %q prefix", err.Error(), "tracker: comments:")
		}
		assertErrorsAsGitHubErrorResponse(t, err)
	})
}

func TestCommentContainsIgnoresOtherAuthors(t *testing.T) {
	const needle = "<!-- zing:done t9 -->"

	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"login":%q}`, testViewerLoginGH)
	})
	mux.HandleFunc("/repos/o/r/issues/42/comments", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"user":{"login":"impostor"},"body":%q}]`, needle)
	})
	g := newDefaultTracker(t, mux)

	found, err := g.CommentContains(t.Context(), testProject, "42", needle)
	if err != nil {
		t.Fatalf("CommentContains: unexpected error: %v", err)
	}
	if found {
		t.Error("CommentContains = true, want false (the marker is from a spoofed author, not the tracker's own login)")
	}
}
