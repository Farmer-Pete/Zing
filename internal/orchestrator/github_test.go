package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"
)

// testGHToken is the token every newTestGHClient build authenticates with,
// so a test can assert the Authorization header it produces.
const testGHToken = "test-token-123" //nolint:gosec // not a credential, a fixed test fixture value

// newTestGHClient builds a real ghClient whose *github.Client points at an
// httptest server backed by mux, so github_test.go exercises the real
// go-github v92 wiring at the HTTP layer rather than faking the GitHub
// interface. go-github v92's Client keeps its base URL as an unexported
// field with no plain setter, so this uses the github.WithURLs client
// option -- the same mechanism go-github's own tests use to point a Client
// at an httptest server -- rather than assigning a BaseURL field directly.
func newTestGHClient(t *testing.T, mux *http.ServeMux) ghClient {
	t.Helper()

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	serverURL := server.URL + "/"
	c, err := github.NewClient(github.WithAuthToken(testGHToken), github.WithURLs(&serverURL, &serverURL))
	if err != nil {
		t.Fatalf("github.NewClient: %v", err)
	}

	return ghClient{c: c}
}

func TestNewGitHub(t *testing.T) {
	t.Run("empty token is an error", func(t *testing.T) {
		_, err := NewGitHub("")
		if err == nil {
			t.Fatal("NewGitHub(\"\"): expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "token must not be empty") {
			t.Errorf("NewGitHub(\"\") error = %q, want it to mention %q", err.Error(), "token must not be empty")
		}
	})

	t.Run("a non-empty token succeeds", func(t *testing.T) {
		gh, err := NewGitHub("a-token")
		if err != nil {
			t.Fatalf("NewGitHub: unexpected error: %v", err)
		}
		if gh == nil {
			t.Fatal("NewGitHub: expected a non-nil GitHub")
		}
	})
}

func TestGHClientRepoDefaultBranch(t *testing.T) {
	var gotAuth string

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"default_branch": "main"}`)
	})

	g := newTestGHClient(t, mux)

	branch, err := g.RepoDefaultBranch(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("RepoDefaultBranch: unexpected error: %v", err)
	}
	if branch != mainBranch {
		t.Errorf("RepoDefaultBranch = %q, want %q", branch, mainBranch)
	}
	if want := "Bearer " + testGHToken; gotAuth != want {
		t.Errorf("Authorization header = %q, want %q", gotAuth, want)
	}
}

func TestGHClientCreateDraftPR(t *testing.T) {
	var (
		gotMethod string
		gotAuth   string
		gotBody   map[string]any
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		// The httptest server invokes this handler on its own goroutine, so
		// a failure here must be recorded with t.Errorf (goroutine-safe for
		// marking the test failed), not t.Fatalf (PR review fix: t.Fatalf
		// calls runtime.Goexit, which off the main goroutine only kills that
		// goroutine, not the test, and is unsafe to call concurrently with
		// the main goroutine's own use of t).
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		fmt.Fprint(w, `{"html_url": "https://github.com/acme/widgets/pull/42", "number": 42}`)
	})

	g := newTestGHClient(t, mux)

	url, number, err := g.CreateDraftPR(t.Context(), "acme", "widgets", "zing/1-slug", mainBranch, "A title", "A body")
	if err != nil {
		t.Fatalf("CreateDraftPR: unexpected error: %v", err)
	}
	if url != "https://github.com/acme/widgets/pull/42" {
		t.Errorf("url = %q, want %q", url, "https://github.com/acme/widgets/pull/42")
	}
	if number != 42 {
		t.Errorf("number = %d, want 42", number)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
	}
	if want := "Bearer " + testGHToken; gotAuth != want {
		t.Errorf("Authorization header = %q, want %q", gotAuth, want)
	}
	if draft, ok := gotBody["draft"].(bool); !ok || !draft {
		t.Errorf("request body draft = %v, want true", gotBody["draft"])
	}
	if gotBody["head"] != "zing/1-slug" {
		t.Errorf("request body head = %v, want %q", gotBody["head"], "zing/1-slug")
	}
	if gotBody["base"] != mainBranch {
		t.Errorf("request body base = %v, want %q", gotBody["base"], mainBranch)
	}
}

func TestGHClientRequiredChecks(t *testing.T) {
	t.Run("unions modern checks and legacy contexts, deduplicated", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/branches/main/protection", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{
				"required_status_checks": {
					"strict": true,
					"contexts": ["lint", "ci"],
					"checks": [{"context": "ci"}, {"context": "build"}]
				}
			}`)
		})

		g := newTestGHClient(t, mux)

		checks, err := g.RequiredChecks(t.Context(), "acme", "widgets", mainBranch)
		if err != nil {
			t.Fatalf("RequiredChecks: unexpected error: %v", err)
		}

		want := map[string]bool{"ci": true, "build": true, "lint": true}
		if len(checks) != len(want) {
			t.Fatalf("RequiredChecks = %v, want the %d contexts %v (deduplicated)", checks, len(want), want)
		}
		for _, c := range checks {
			if !want[c] {
				t.Errorf("RequiredChecks contains unexpected context %q, got %v", c, checks)
			}
		}
	})

	t.Run("branch not protected yields an empty slice and no error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/branches/main/protection", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message": "Branch not protected"}`)
		})

		g := newTestGHClient(t, mux)

		checks, err := g.RequiredChecks(t.Context(), "acme", "widgets", mainBranch)
		if err != nil {
			t.Fatalf("RequiredChecks: unexpected error: %v", err)
		}
		if len(checks) != 0 {
			t.Errorf("RequiredChecks = %v, want an empty slice", checks)
		}
	})

	t.Run("a different 404 is an error, not an empty result", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/branches/main/protection", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message": "Not Found"}`)
		})

		g := newTestGHClient(t, mux)

		_, err := g.RequiredChecks(t.Context(), "acme", "widgets", mainBranch)
		if err == nil {
			t.Fatal("RequiredChecks: expected an error for a non-branch-protection 404, got nil")
		}
	})
}

func TestGHClientFindPRByHead(t *testing.T) {
	t.Run("returns the match", func(t *testing.T) {
		var gotHead, gotBase, gotState string

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
			gotHead = r.URL.Query().Get("head")
			gotBase = r.URL.Query().Get("base")
			gotState = r.URL.Query().Get("state")
			fmt.Fprint(w, `[{"html_url": "https://github.com/acme/widgets/pull/7", "number": 7}]`)
		})

		g := newTestGHClient(t, mux)

		url, number, ok, err := g.FindPRByHead(t.Context(), "acme", "widgets", "zing/1-slug", mainBranch)
		if err != nil {
			t.Fatalf("FindPRByHead: unexpected error: %v", err)
		}
		if !ok {
			t.Fatal("FindPRByHead: ok = false, want true")
		}
		if url != "https://github.com/acme/widgets/pull/7" {
			t.Errorf("url = %q, want %q", url, "https://github.com/acme/widgets/pull/7")
		}
		if number != 7 {
			t.Errorf("number = %d, want 7", number)
		}
		if gotHead != "acme:zing/1-slug" {
			t.Errorf("head query = %q, want %q", gotHead, "acme:zing/1-slug")
		}
		if gotBase != mainBranch {
			t.Errorf("base query = %q, want %q", gotBase, mainBranch)
		}
		if gotState != "open" {
			t.Errorf("state query = %q, want %q", gotState, "open")
		}
	})

	t.Run("no match returns ok=false and no error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `[]`)
		})

		g := newTestGHClient(t, mux)

		_, _, ok, err := g.FindPRByHead(t.Context(), "acme", "widgets", "zing/1-slug", mainBranch)
		if err != nil {
			t.Fatalf("FindPRByHead: unexpected error: %v", err)
		}
		if ok {
			t.Error("FindPRByHead: ok = true, want false")
		}
	})

	// PR review finding L: OpenDraftPR's fallback used to ignore base
	// entirely, so it could return an open PR into some other base branch.
	// This mimics GitHub's own Head+Base filtering (the real endpoint drops
	// a PR whose base does not match) to prove FindPRByHead, given a base
	// that does not match the PR on the server, comes back ok=false.
	t.Run("a PR whose base does not match the requested base is not returned", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("base") != mainBranch {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprint(w, `[{"html_url": "https://github.com/acme/widgets/pull/7", "number": 7}]`)
		})

		g := newTestGHClient(t, mux)

		_, _, ok, err := g.FindPRByHead(t.Context(), "acme", "widgets", "zing/1-slug", "release")
		if err != nil {
			t.Fatalf("FindPRByHead: unexpected error: %v", err)
		}
		if ok {
			t.Error("FindPRByHead: ok = true, want false for a base that does not match the open PR's base")
		}
	})

	t.Run("passes base as its own query parameter, not just head", func(t *testing.T) {
		var gotBase string

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
			gotBase = r.URL.Query().Get("base")
			fmt.Fprint(w, `[]`)
		})

		g := newTestGHClient(t, mux)

		if _, _, _, err := g.FindPRByHead(t.Context(), "acme", "widgets", "zing/1-slug", "release"); err != nil {
			t.Fatalf("FindPRByHead: unexpected error: %v", err)
		}
		if gotBase != "release" {
			t.Errorf("base query = %q, want %q", gotBase, "release")
		}
	})
}
