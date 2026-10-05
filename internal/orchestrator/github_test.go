package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
)

// testGHToken is the token every newTestGHClient build authenticates with,
// so a test can assert the Authorization header it produces.
const testGHToken = "test-token-123" //nolint:gosec // not a credential, a fixed test fixture value

// testDeployCheck is a check-run or required-check name reused across
// several fixtures below, pulled out as a constant so goconst does not
// flag the repeats.
const testDeployCheck = "deploy"

// newTestGHClient builds a real *GitHubClient whose *github.Client points at
// an httptest server backed by mux, so github_test.go exercises the real
// go-github v92 wiring at the HTTP layer rather than faking the GitHub
// interface. go-github v92's Client keeps its base URL as an unexported
// field with no plain setter, so this uses the github.WithURLs client
// option -- the same mechanism go-github's own tests use to point a Client
// at an httptest server -- rather than assigning a BaseURL field directly.
func newTestGHClient(t *testing.T, mux *http.ServeMux) *GitHubClient {
	t.Helper()

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	serverURL := server.URL + "/"
	c, err := github.NewClient(github.WithAuthToken(testGHToken), github.WithURLs(&serverURL, &serverURL))
	if err != nil {
		t.Fatalf("github.NewClient: %v", err)
	}

	return &GitHubClient{c: c}
}

func TestNewGitHub(t *testing.T) {
	t.Parallel()
	t.Run("empty token is an error", func(t *testing.T) {
		t.Parallel()
		_, err := NewGitHub("")
		if err == nil {
			t.Fatal("NewGitHub(\"\"): expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "token must not be empty") {
			t.Errorf("NewGitHub(\"\") error = %q, want it to mention %q", err.Error(), "token must not be empty")
		}
	})

	t.Run("a non-empty token succeeds", func(t *testing.T) {
		t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestGHClientCreateDraftPRClassifiesErrors proves CreateDraftPR routes a
// failed PullRequests.Create through classifyGitHubErr (PKG9-PLAN.md
// section 10.3), the same as GetPR and the other M3 task 2 methods, so
// PUBLISH (internal/job/shipping.go) can tell auth from unavailable from
// rate limit on a live failure instead of only ever seeing a plain wrapped
// error.
func TestGHClientCreateDraftPRClassifiesErrors(t *testing.T) {
	t.Parallel()

	t.Run("401 is ErrGitHubAuth", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message": "Bad credentials"}`)
		})
		g := newTestGHClient(t, mux)

		_, _, err := g.CreateDraftPR(t.Context(), "acme", "widgets", "zing/1-slug", mainBranch, "A title", "A body")
		if !errors.Is(err, ErrGitHubAuth) {
			t.Errorf("error = %v, want errors.Is(err, ErrGitHubAuth)", err)
		}
	})

	t.Run("502 is ErrGitHubUnavailable", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"message": "Bad Gateway"}`)
		})
		g := newTestGHClient(t, mux)

		_, _, err := g.CreateDraftPR(t.Context(), "acme", "widgets", "zing/1-slug", mainBranch, "A title", "A body")
		if !errors.Is(err, ErrGitHubUnavailable) {
			t.Errorf("error = %v, want errors.Is(err, ErrGitHubUnavailable)", err)
		}
	})

	t.Run("429 is RateLimitedError", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message": "too many requests"}`)
		})
		g := newTestGHClient(t, mux)

		_, _, err := g.CreateDraftPR(t.Context(), "acme", "widgets", "zing/1-slug", mainBranch, "A title", "A body")
		rle, ok := errors.AsType[RateLimitedError](err)
		if !ok {
			t.Fatalf("error = %v, want errors.As(err, *RateLimitedError)", err)
		}
		if rle.ResetAt.IsZero() {
			t.Errorf("ResetAt = %v, want a non-zero reset time from Retry-After", rle.ResetAt)
		}
	})
}

// handleNoRules registers a 404 handler for mainBranch's rules/branches
// route on mux, the response a repository with no applicable ruleset
// returns (D28): every TestGHClientRequiredChecks and TestRequiredCheckRules
// subtest that does not itself script that route needs one registered, so
// an unmatched request fails loudly (http.ServeMux's own plain-text 404)
// rather than silently reading as "no rules" by accident.
func handleNoRules(mux *http.ServeMux) {
	mux.HandleFunc("/repos/acme/widgets/rules/branches/main", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message": "Not Found"}`)
	})
}

// handleNoProtection registers a 404 "Branch not protected" handler for
// mainBranch's classic protection route on mux (D28; see handleNoRules).
func handleNoProtection(mux *http.ServeMux) {
	mux.HandleFunc("/repos/acme/widgets/branches/main/protection", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message": "Branch not protected"}`)
	})
}

func TestGHClientRequiredChecks(t *testing.T) {
	t.Parallel()
	t.Run("unions classic checks, legacy contexts, and a ruleset's checks, deduplicated", func(t *testing.T) {
		t.Parallel()
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
		mux.HandleFunc("/repos/acme/widgets/rules/branches/main", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `[{
				"ruleset_id": 1,
				"type": "required_status_checks",
				"parameters": {"required_status_checks": [{"context": "ci"}, {"context": "deploy"}]}
			}]`)
		})

		g := newTestGHClient(t, mux)

		checks, err := g.RequiredChecks(t.Context(), "acme", "widgets", mainBranch)
		if err != nil {
			t.Fatalf("RequiredChecks: unexpected error: %v", err)
		}

		want := map[string]bool{"ci": true, "build": true, "lint": true, testDeployCheck: true}
		if len(checks) != len(want) {
			t.Fatalf("RequiredChecks = %v, want the %d contexts %v (deduplicated)", checks, len(want), want)
		}
		for _, c := range checks {
			if !want[c] {
				t.Errorf("RequiredChecks contains unexpected context %q, got %v", c, checks)
			}
		}
	})

	t.Run("neither source names a check yields an empty slice and no error", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		handleNoProtection(mux)
		handleNoRules(mux)

		g := newTestGHClient(t, mux)

		checks, err := g.RequiredChecks(t.Context(), "acme", "widgets", mainBranch)
		if err != nil {
			t.Fatalf("RequiredChecks: unexpected error: %v", err)
		}
		if len(checks) != 0 {
			t.Errorf("RequiredChecks = %v, want an empty slice", checks)
		}
	})

	t.Run("a non-404 failure from classic protection is an error, not swallowed", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/branches/main/protection", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message": "Internal Server Error"}`)
		})
		handleNoRules(mux)

		g := newTestGHClient(t, mux)

		_, err := g.RequiredChecks(t.Context(), "acme", "widgets", mainBranch)
		if err == nil {
			t.Fatal("RequiredChecks: expected an error for a 500 from classic protection, got nil")
		}
	})
}

func TestGHClientFindPRByHead(t *testing.T) {
	t.Parallel()
	t.Run("returns the match", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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

// TestGHClientFindPRByHeadClassifiesErrors proves FindPRByHead routes a
// failed PullRequests.List through classifyGitHubErr (PKG9-PLAN.md section
// 10.3), the same as GetPR and the other M3 task 2 methods, so PUBLISH
// (internal/job/shipping.go) can tell auth from unavailable from rate limit
// on a live failure instead of only ever seeing a plain wrapped error. The
// not-found case (an empty list, ok=false, no error) stays covered by
// TestGHClientFindPRByHead above and is untouched by this change.
func TestGHClientFindPRByHeadClassifiesErrors(t *testing.T) {
	t.Parallel()

	t.Run("401 is ErrGitHubAuth", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message": "Bad credentials"}`)
		})
		g := newTestGHClient(t, mux)

		_, _, _, err := g.FindPRByHead(t.Context(), "acme", "widgets", "zing/1-slug", mainBranch)
		if !errors.Is(err, ErrGitHubAuth) {
			t.Errorf("error = %v, want errors.Is(err, ErrGitHubAuth)", err)
		}
	})

	t.Run("502 is ErrGitHubUnavailable", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"message": "Bad Gateway"}`)
		})
		g := newTestGHClient(t, mux)

		_, _, _, err := g.FindPRByHead(t.Context(), "acme", "widgets", "zing/1-slug", mainBranch)
		if !errors.Is(err, ErrGitHubUnavailable) {
			t.Errorf("error = %v, want errors.Is(err, ErrGitHubUnavailable)", err)
		}
	})

	t.Run("429 is RateLimitedError", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message": "too many requests"}`)
		})
		g := newTestGHClient(t, mux)

		_, _, _, err := g.FindPRByHead(t.Context(), "acme", "widgets", "zing/1-slug", mainBranch)
		rle, ok := errors.AsType[RateLimitedError](err)
		if !ok {
			t.Fatalf("error = %v, want errors.As(err, *RateLimitedError)", err)
		}
		if rle.ResetAt.IsZero() {
			t.Errorf("ResetAt = %v, want a non-zero reset time from Retry-After", rle.ResetAt)
		}
	})
}

// --- PKG9-PLAN.md section 10.3, M3 task 2: GitHub REST reads and writes ---

// TestGitHubClientSatisfiesInterfaces documents the GitHub compile-time
// assertion next to GitHubClient's own declaration (github.go): GitHubClient
// keeps satisfying the four-method GitHub interface. The matching
// assertions for job.PullRequests and job.Checks live in
// internal/job/shipping_test.go instead of here: internal/job already
// imports internal/orchestrator in its regular (non-test) code, and an
// internal (same-package) test file in internal/orchestrator cannot import
// internal/job without an import cycle, even though an external
// zing/internal/job test package legitimately imports zing/internal/orchestrator.
// No GraphQL method exists yet (M4 task 1 adds DraftFlips and ReviewThreads).
func TestGitHubClientSatisfiesInterfaces(t *testing.T) {
	t.Parallel()
}

// TestGetPR proves GetPR fills PRState from GitHub's own fields, including
// MergeCommitSHA from merge_commit_sha once the pull request is merged.
func TestGetPR(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls/42", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{
			"number": 42,
			"node_id": "PR_kwABC",
			"state": "open",
			"merged": false,
			"draft": true,
			"head": {"sha": "deadbeefcafe0000111122223333444455556666"},
			"base": {"ref": "main"}
		}`)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/63", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{
			"number": 63,
			"node_id": "PR_kwXYZ",
			"state": "closed",
			"merged": true,
			"draft": false,
			"head": {"sha": "deadbeefcafe0000111122223333444455556666"},
			"base": {"ref": "main"},
			"merge_commit_sha": "0123456789abcdef0123456789abcdef01234567"
		}`)
	})

	g := newTestGHClient(t, mux)

	got, err := g.GetPR(t.Context(), "acme", "widgets", 42)
	if err != nil {
		t.Fatalf("GetPR: unexpected error: %v", err)
	}
	want := PRState{
		Number:  42,
		NodeID:  "PR_kwABC",
		State:   "open",
		Merged:  false,
		Draft:   true,
		HeadSHA: "deadbeefcafe0000111122223333444455556666",
		BaseRef: "main",
	}
	if got != want {
		t.Errorf("GetPR = %+v, want %+v", got, want)
	}

	gotMerged, err := g.GetPR(t.Context(), "acme", "widgets", 63)
	if err != nil {
		t.Fatalf("GetPR: unexpected error: %v", err)
	}
	if gotMerged.MergeCommitSHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("GetPR().MergeCommitSHA = %q, want the merge commit sha", gotMerged.MergeCommitSHA)
	}
}

func TestListCheckRunsAllPages(t *testing.T) {
	t.Parallel()

	var gotFilter, gotPerPage string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/commits/sha1/check-runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{"total_count": 1, "check_runs": [
				{"id": 2, "name": "deploy", "status": "completed", "conclusion": "neutral", "app": {"slug": "other-app", "id": 99}, "details_url": "https://x/2"}
			]}`)
			return
		}
		gotFilter = r.URL.Query().Get("filter")
		gotPerPage = r.URL.Query().Get("per_page")
		w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/acme/widgets/commits/sha1/check-runs?page=2>; rel="next"`)
		fmt.Fprint(w, `{"total_count": 1, "check_runs": [
			{"id": 1, "name": "ci", "status": "completed", "conclusion": "success", "app": {"slug": "github-actions", "id": 15368}, "details_url": "https://x/1"}
		]}`)
	})

	g := newTestGHClient(t, mux)

	got, err := g.ListCheckRuns(t.Context(), "acme", "widgets", "sha1")
	if err != nil {
		t.Fatalf("ListCheckRuns: unexpected error: %v", err)
	}
	if gotFilter != "latest" {
		t.Errorf("filter query = %q, want %q", gotFilter, "latest")
	}
	if gotPerPage != strconv.Itoa(ghPerPage) {
		t.Errorf("per_page query = %q, want %q", gotPerPage, strconv.Itoa(ghPerPage))
	}
	want := []CheckRun{
		{ID: 1, Name: "ci", Status: "completed", Conclusion: "success", AppSlug: "github-actions", AppID: 15368, DetailsURL: "https://x/1"},
		{ID: 2, Name: testDeployCheck, Status: "completed", Conclusion: "neutral", AppSlug: "other-app", AppID: 99, DetailsURL: "https://x/2"},
	}
	if len(got) != len(want) {
		t.Fatalf("ListCheckRuns = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListCheckRuns[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestListStatusesAllPages(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/commits/sha1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{"state": "pending", "statuses": [
				{"context": "deploy/preview", "state": "pending", "target_url": "https://x/deploy"}
			]}`)
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/acme/widgets/commits/sha1/status?page=2>; rel="next"`)
		fmt.Fprint(w, `{"state": "success", "statuses": [
			{"context": "ci", "state": "success", "target_url": "https://x/ci"}
		]}`)
	})

	g := newTestGHClient(t, mux)

	got, err := g.ListStatuses(t.Context(), "acme", "widgets", "sha1")
	if err != nil {
		t.Fatalf("ListStatuses: unexpected error: %v", err)
	}
	want := []CommitStatus{
		{Context: "ci", State: "success", TargetURL: "https://x/ci"},
		{Context: "deploy/preview", State: "pending", TargetURL: "https://x/deploy"},
	}
	if len(got) != len(want) {
		t.Fatalf("ListStatuses = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListStatuses[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// assertRequiredCheckRules fails t unless got holds exactly the (context,
// app id) pairs want names, order ignored.
func assertRequiredCheckRules(t *testing.T, got []RequiredCheck, want map[string]*int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("RequiredCheckRules = %+v, want %d entries %v", got, len(want), want)
	}
	for _, rc := range got {
		wantAppID, ok := want[rc.Context]
		if !ok {
			t.Errorf("RequiredCheckRules: unexpected context %q", rc.Context)
			continue
		}
		if (rc.AppID == nil) != (wantAppID == nil) {
			t.Errorf("RequiredCheckRules[%q].AppID = %v, want %v", rc.Context, rc.AppID, wantAppID)
			continue
		}
		if rc.AppID != nil && *rc.AppID != *wantAppID {
			t.Errorf("RequiredCheckRules[%q].AppID = %d, want %d", rc.Context, *rc.AppID, *wantAppID)
		}
	}
}

// TestRequiredCheckRules proves D28's merge of classic branch protection
// with the rules a repository ruleset applies to the branch
// (GET /repos/{owner}/{repo}/rules/branches/{branch}).
func TestRequiredCheckRules(t *testing.T) {
	t.Parallel()

	t.Run("classic only: legacy context, a modern app-bound check, and app id -1 or absent as any source", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/branches/main/protection", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{
				"required_status_checks": {
					"contexts": ["legacy-ctx"],
					"checks": [
						{"context": "ci", "app_id": 15368},
						{"context": "format", "app_id": -1},
						{"context": "build"}
					]
				}
			}`)
		})
		handleNoRules(mux)

		g := newTestGHClient(t, mux)

		got, err := g.RequiredCheckRules(t.Context(), "acme", "widgets", mainBranch)
		if err != nil {
			t.Fatalf("RequiredCheckRules: unexpected error: %v", err)
		}
		assertRequiredCheckRules(t, got, map[string]*int64{"ci": new(int64(15368)), "format": nil, "build": nil, "legacy-ctx": nil})
	})

	t.Run("ruleset only: the Zing shape, app ids kept per check", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		handleNoProtection(mux)
		// The payload Farmer-Pete/Zing's main returns from
		// GET /repos/Farmer-Pete/Zing/rules/branches/main (facts verified
		// 2026-10-01): one required_status_checks rule from ruleset 23792541,
		// two checks sharing app 15368 and two each bound to their own app.
		mux.HandleFunc("/repos/acme/widgets/rules/branches/main", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `[{
				"ruleset_id": 23792541,
				"type": "required_status_checks",
				"parameters": {"required_status_checks": [
					{"context": "Hooks and tests", "integration_id": 15368},
					{"context": "Secret scan", "integration_id": 15368},
					{"context": "CodeRabbit", "integration_id": 347564},
					{"context": "cubic · AI code reviewer", "integration_id": 1082092}
				]}
			}]`)
		})

		g := newTestGHClient(t, mux)

		got, err := g.RequiredCheckRules(t.Context(), "acme", "widgets", mainBranch)
		if err != nil {
			t.Fatalf("RequiredCheckRules: unexpected error: %v", err)
		}
		assertRequiredCheckRules(t, got, map[string]*int64{
			"Hooks and tests":          new(int64(15368)),
			"Secret scan":              new(int64(15368)),
			"CodeRabbit":               new(int64(347564)),
			"cubic · AI code reviewer": new(int64(1082092)),
		})
	})

	t.Run("both sources, an exact duplicate dropped", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/branches/main/protection", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{
				"required_status_checks": {
					"contexts": [],
					"checks": [{"context": "ci", "app_id": 15368}, {"context": "lint"}]
				}
			}`)
		})
		mux.HandleFunc("/repos/acme/widgets/rules/branches/main", func(w http.ResponseWriter, _ *http.Request) {
			// "ci"/15368 exactly duplicates the classic entry and must be
			// dropped; "deploy"/42 is new and must be kept.
			fmt.Fprint(w, `[{
				"ruleset_id": 1,
				"type": "required_status_checks",
				"parameters": {"required_status_checks": [
					{"context": "ci", "integration_id": 15368},
					{"context": "deploy", "integration_id": 42}
				]}
			}]`)
		})

		g := newTestGHClient(t, mux)

		got, err := g.RequiredCheckRules(t.Context(), "acme", "widgets", mainBranch)
		if err != nil {
			t.Fatalf("RequiredCheckRules: unexpected error: %v", err)
		}
		assertRequiredCheckRules(t, got, map[string]*int64{"ci": new(int64(15368)), "lint": nil, testDeployCheck: new(int64(42))})
	})

	t.Run("neither source names a check (a valid empty response from each) yields an empty slice and no error", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/branches/main/protection", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"required_status_checks": {"contexts": [], "checks": []}}`)
		})
		mux.HandleFunc("/repos/acme/widgets/rules/branches/main", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `[]`)
		})

		g := newTestGHClient(t, mux)

		got, err := g.RequiredCheckRules(t.Context(), "acme", "widgets", mainBranch)
		if err != nil {
			t.Fatalf("RequiredCheckRules: unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("RequiredCheckRules = %+v, want an empty slice", got)
		}
	})

	t.Run("a 404 on each source yields an empty slice and no error", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		handleNoProtection(mux)
		handleNoRules(mux)

		g := newTestGHClient(t, mux)

		got, err := g.RequiredCheckRules(t.Context(), "acme", "widgets", mainBranch)
		if err != nil {
			t.Fatalf("RequiredCheckRules: unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("RequiredCheckRules = %+v, want an empty slice", got)
		}
	})

	t.Run("a non-404 failure from the rules source is an error, not swallowed", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		handleNoProtection(mux)
		mux.HandleFunc("/repos/acme/widgets/rules/branches/main", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message": "Internal Server Error"}`)
		})

		g := newTestGHClient(t, mux)

		_, err := g.RequiredCheckRules(t.Context(), "acme", "widgets", mainBranch)
		if err == nil {
			t.Fatal("RequiredCheckRules: expected an error for a 500 from the rules source, got nil")
		}
	})
}

func TestListReviews(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"user": {"login": "zing-bot", "type": "Bot"}, "state": "COMMENTED", "commit_id": "sha2"}]`)
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/acme/widgets/pulls/7/reviews?page=2>; rel="next"`)
		fmt.Fprint(w, `[{"user": {"login": "octocat", "type": "User"}, "state": "APPROVED", "commit_id": "sha1"}]`)
	})

	g := newTestGHClient(t, mux)

	got, err := g.ListReviews(t.Context(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("ListReviews: unexpected error: %v", err)
	}
	want := []Review{
		{Login: "octocat", UserType: "User", State: "APPROVED", CommitID: "sha1"},
		{Login: "zing-bot", UserType: "Bot", State: "COMMENTED", CommitID: "sha2"},
	}
	if len(got) != len(want) {
		t.Fatalf("ListReviews = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListReviews[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRequestReviewers(t *testing.T) {
	t.Parallel()

	var gotMethod string
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls/7/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		fmt.Fprint(w, `{"number": 7}`)
	})

	g := newTestGHClient(t, mux)

	if err := g.RequestReviewers(t.Context(), "acme", "widgets", 7, "octocat"); err != nil {
		t.Fatalf("RequestReviewers: unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
	}
	reviewers, ok := gotBody["reviewers"].([]any)
	if !ok || len(reviewers) != 1 || reviewers[0] != "octocat" {
		t.Errorf("request body reviewers = %v, want [\"octocat\"]", gotBody["reviewers"])
	}
}

func TestCommentOnPR(t *testing.T) {
	t.Parallel()

	t.Run("posts the body as an issue comment", func(t *testing.T) {
		t.Parallel()
		var gotMethod string
		var gotBody map[string]any
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/issues/63/comments", func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode request body: %v", err)
				return
			}
			fmt.Fprint(w, `{"id": 1}`)
		})

		g := newTestGHClient(t, mux)

		if err := g.CommentOnPR(t.Context(), "acme", "widgets", 63, "@coderabbitai review"); err != nil {
			t.Fatalf("CommentOnPR: unexpected error: %v", err)
		}
		if gotMethod != http.MethodPost {
			t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
		}
		if gotBody["body"] != "@coderabbitai review" {
			t.Errorf("request body body = %v, want %q", gotBody["body"], "@coderabbitai review")
		}
	})

	t.Run("401 is ErrGitHubAuth", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/issues/63/comments", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message": "Bad credentials"}`)
		})
		g := newTestGHClient(t, mux)

		err := g.CommentOnPR(t.Context(), "acme", "widgets", 63, "@coderabbitai review")
		if !errors.Is(err, ErrGitHubAuth) {
			t.Errorf("error = %v, want errors.Is(err, ErrGitHubAuth)", err)
		}
	})
}

// TestRerunJobPostsJobRerun checks GitHubClient.RerunJob against the same
// httptest setup as the JobLogTail tests (newTestGHClient): a POST to the
// job's own rerun endpoint, and a non-2xx response classified as an error.
func TestRerunJobPostsJobRerun(t *testing.T) {
	t.Parallel()

	t.Run("sends exactly one POST to the job's rerun endpoint", func(t *testing.T) {
		t.Parallel()
		var gotMethod string
		var hits int
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/actions/jobs/99/rerun", func(w http.ResponseWriter, r *http.Request) {
			hits++
			gotMethod = r.Method
			w.WriteHeader(http.StatusCreated)
		})

		g := newTestGHClient(t, mux)

		if err := g.RerunJob(t.Context(), "acme", "widgets", 99); err != nil {
			t.Fatalf("RerunJob: unexpected error: %v", err)
		}
		if hits != 1 {
			t.Errorf("rerun endpoint hits = %d, want 1", hits)
		}
		if gotMethod != http.MethodPost {
			t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
		}
	})

	t.Run("403 is a non-nil error", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/actions/jobs/99/rerun", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message": "Resource not accessible by integration"}`)
		})
		g := newTestGHClient(t, mux)

		if err := g.RerunJob(t.Context(), "acme", "widgets", 99); err == nil {
			t.Error("RerunJob: expected an error, got nil")
		}
	})
}

func TestMergePinsSha(t *testing.T) {
	t.Parallel()

	t.Run("the request carries sha, method, and title; a merged result returns its sha", func(t *testing.T) {
		t.Parallel()
		var gotMethod string
		var gotBody map[string]any

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls/7/merge", func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode request body: %v", err)
				return
			}
			fmt.Fprint(w, `{"sha": "merged0000111122223333444455556666deadbe", "merged": true, "message": "Pull Request successfully merged"}`)
		})

		g := newTestGHClient(t, mux)

		sha, err := g.Merge(t.Context(), "acme", "widgets", 7, "deadbeef", "squash", "A title")
		if err != nil {
			t.Fatalf("Merge: unexpected error: %v", err)
		}
		if sha != "merged0000111122223333444455556666deadbe" {
			t.Errorf("Merge sha = %q, want %q", sha, "merged0000111122223333444455556666deadbe")
		}
		if gotMethod != http.MethodPut {
			t.Errorf("method = %q, want %q", gotMethod, http.MethodPut)
		}
		if gotBody["sha"] != "deadbeef" {
			t.Errorf("request body sha = %v, want %q", gotBody["sha"], "deadbeef")
		}
		if gotBody["merge_method"] != "squash" {
			t.Errorf("request body merge_method = %v, want %q", gotBody["merge_method"], "squash")
		}
		if gotBody["commit_title"] != "A title" {
			t.Errorf("request body commit_title = %v, want %q", gotBody["commit_title"], "A title")
		}
	})

	t.Run("merged false is ErrMergeRefused", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/pulls/7/merge", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"merged": false, "message": "Head branch was modified"}`)
		})

		g := newTestGHClient(t, mux)

		_, err := g.Merge(t.Context(), "acme", "widgets", 7, "deadbeef", "squash", "A title")
		if !errors.Is(err, ErrMergeRefused) {
			t.Errorf("Merge error = %v, want errors.Is(err, ErrMergeRefused)", err)
		}
	})

	for _, code := range []int{http.StatusMethodNotAllowed, http.StatusConflict} {
		t.Run(fmt.Sprintf("status %d is ErrMergeRefused", code), func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/acme/widgets/pulls/7/merge", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				fmt.Fprint(w, `{"message": "refused"}`)
			})

			g := newTestGHClient(t, mux)

			_, err := g.Merge(t.Context(), "acme", "widgets", 7, "deadbeef", "squash", "A title")
			if !errors.Is(err, ErrMergeRefused) {
				t.Errorf("Merge error = %v, want errors.Is(err, ErrMergeRefused)", err)
			}
		})
	}
}

func TestViewer(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"login": "farmer-pete"}`)
	})

	g := newTestGHClient(t, mux)

	got, err := g.Viewer(t.Context())
	if err != nil {
		t.Fatalf("Viewer: unexpected error: %v", err)
	}
	if got != "farmer-pete" {
		t.Errorf("Viewer = %q, want %q", got, "farmer-pete")
	}
}

// fillerLog writes n bytes of repeated, newline-terminated filler to w, in
// bulk chunks rather than one byte or line at a time, so TestJobLogTailLast200's
// 64 MiB case runs fast. A write failure just stops early and logs: t.Logf,
// not t.Fatalf, because the httptest server runs the handler that calls this
// on its own goroutine.
func fillerLog(t *testing.T, w io.Writer, n int) {
	t.Helper()
	chunk := bytes.Repeat([]byte("0123456789\n"), 1<<20/11+1)[:1<<20]
	written := 0
	for written < n {
		take := len(chunk)
		if written+take > n {
			take = n - written
		}
		if _, err := w.Write(chunk[:take]); err != nil {
			t.Logf("fillerLog: write: %v", err)
			return
		}
		written += take
	}
}

func TestJobLogTailLast200(t *testing.T) {
	t.Parallel()

	t.Run("returns the last n lines, fetched with no Authorization header", func(t *testing.T) {
		t.Parallel()
		var gotAuthOnLogFetch string
		var sawLogFetch bool

		logServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sawLogFetch = true
			gotAuthOnLogFetch = r.Header.Get("Authorization")
			for i := 1; i <= 5; i++ {
				fmt.Fprintf(w, "line%d\n", i)
			}
		}))
		t.Cleanup(logServer.Close)

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/actions/jobs/99/logs", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", logServer.URL+"/log")
			w.WriteHeader(http.StatusFound)
		})

		g := newTestGHClient(t, mux)

		got, err := g.JobLogTail(t.Context(), "acme", "widgets", 99, 3)
		if err != nil {
			t.Fatalf("JobLogTail: unexpected error: %v", err)
		}
		if !sawLogFetch {
			t.Fatal("JobLogTail: the signed log URL was never fetched")
		}
		if gotAuthOnLogFetch != "" {
			t.Errorf("Authorization header on the log fetch = %q, want none", gotAuthOnLogFetch)
		}
		if want := "line3\nline4\nline5"; got != want {
			t.Errorf("JobLogTail = %q, want %q", got, want)
		}
	})

	t.Run("cuts a line longer than 64 KiB", func(t *testing.T) {
		t.Parallel()
		logServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, strings.Repeat("a", 70000)+"\nshort-line")
		}))
		t.Cleanup(logServer.Close)

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/actions/jobs/99/logs", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", logServer.URL+"/log")
			w.WriteHeader(http.StatusFound)
		})

		g := newTestGHClient(t, mux)

		got, err := g.JobLogTail(t.Context(), "acme", "widgets", 99, 10)
		if err != nil {
			t.Fatalf("JobLogTail: unexpected error: %v", err)
		}
		lines := strings.Split(got, "\n")
		if len(lines) != 2 {
			t.Fatalf("JobLogTail lines = %d, want 2 (%q)", len(lines), got)
		}
		wantFirst := strings.Repeat("a", maxLogLineBytes) + "[line cut]"
		if lines[0] != wantFirst {
			t.Errorf("JobLogTail first line length = %d, want the cut marker ending a %d-byte prefix", len(lines[0]), maxLogLineBytes)
		}
		if lines[1] != "short-line" {
			t.Errorf("JobLogTail second line = %q, want %q", lines[1], "short-line")
		}
	})

	t.Run("stops at 64 MiB and ends with the cut marker", func(t *testing.T) {
		t.Parallel()
		logServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fillerLog(t, w, maxLogTotalBytes+50_000)
		}))
		t.Cleanup(logServer.Close)

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/actions/jobs/99/logs", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", logServer.URL+"/log")
			w.WriteHeader(http.StatusFound)
		})

		g := newTestGHClient(t, mux)

		got, err := g.JobLogTail(t.Context(), "acme", "widgets", 99, 3)
		if err != nil {
			t.Fatalf("JobLogTail: unexpected error: %v", err)
		}
		lines := strings.Split(got, "\n")
		if lines[len(lines)-1] != "[log cut at 64 MiB]" {
			t.Errorf("JobLogTail last line = %q, want %q", lines[len(lines)-1], "[log cut at 64 MiB]")
		}
	})
}

// TestJobLogTailCutsAtFailedStep checks that tailLog quotes the failed
// step's own output -- from its "##[group]Run " header to its last
// "##[error]" line -- rather than whatever happens to be the last n lines
// of the whole job log, which on a real PR (#50) was git's unrelated
// detached-HEAD notice and post-job cleanup.
func TestJobLogTailCutsAtFailedStep(t *testing.T) {
	t.Parallel()

	ts := func(i int) string {
		return fmt.Sprintf("2024-05-01T12:00:%02d.0000000Z", i%60)
	}

	serve := func(t *testing.T, body string) *GitHubClient {
		t.Helper()
		logServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, body)
		}))
		t.Cleanup(logServer.Close)

		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/widgets/actions/jobs/99/logs", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", logServer.URL+"/log")
			w.WriteHeader(http.StatusFound)
		})
		return newTestGHClient(t, mux)
	}

	t.Run("keeps only the failed step, dropping an earlier step and later cleanup", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		fmt.Fprintf(&b, "%s ##[group]Run actions/checkout@v4\n", ts(0))
		fmt.Fprintf(&b, "%s Note: switching to 'a1b2c3d'.\n", ts(1))
		fmt.Fprintf(&b, "%s You are in 'detached HEAD' state. You can look around.\n", ts(2))
		fmt.Fprintf(&b, "%s ##[endgroup]\n", ts(3))
		fmt.Fprintf(&b, "%s ##[group]Run make lint\n", ts(4))
		fmt.Fprintf(&b, "%s golangci-lint run ./...\n", ts(5))
		fmt.Fprintf(&b, "%s ##[error]internal/job/shipping.go:10: unused variable x\n", ts(6))
		fmt.Fprintf(&b, "%s ##[error]internal/job/respond.go:20: unused import\n", ts(7))
		fmt.Fprintf(&b, "%s ##[error]Process completed with exit code 2.\n", ts(8))
		for i := range 300 {
			fmt.Fprintf(&b, "%s Cleaning up orphan processes\n", ts(9+i))
		}

		g := serve(t, b.String())

		got, err := g.JobLogTail(t.Context(), "acme", "widgets", 99, 200)
		if err != nil {
			t.Fatalf("JobLogTail: unexpected error: %v", err)
		}

		lines := strings.Split(got, "\n")
		if !strings.Contains(lines[0], "##[group]Run make lint") {
			t.Errorf("JobLogTail first line = %q, want it to contain %q", lines[0], "##[group]Run make lint")
		}
		if !strings.Contains(lines[len(lines)-1], "##[error]Process completed with exit code 2.") {
			t.Errorf("JobLogTail last line = %q, want it to contain %q", lines[len(lines)-1], "##[error]Process completed with exit code 2.")
		}
		if strings.Contains(got, "detached HEAD") {
			t.Errorf("JobLogTail = %q, must not contain the checkout step's output", got)
		}
		if strings.Contains(got, "Cleaning up orphan processes") {
			t.Errorf("JobLogTail = %q, must not contain the post-job cleanup lines", got)
		}
	})

	t.Run("stops at the next step header, so a later step cannot replace the failure", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		fmt.Fprintf(&b, "%s ##[group]Run make lint\n", ts(0))
		fmt.Fprintf(&b, "%s golangci-lint run ./...\n", ts(1))
		fmt.Fprintf(&b, "%s ##[error]Process completed with exit code 2.\n", ts(2))
		fmt.Fprintf(&b, "%s ##[group]Run post-job cleanup\n", ts(3))
		fmt.Fprintf(&b, "%s removing temp directory\n", ts(4))
		fmt.Fprintf(&b, "%s ##[error]some unrelated later failure\n", ts(5))

		g := serve(t, b.String())

		got, err := g.JobLogTail(t.Context(), "acme", "widgets", 99, 200)
		if err != nil {
			t.Fatalf("JobLogTail: unexpected error: %v", err)
		}

		lines := strings.Split(got, "\n")
		if !strings.Contains(lines[0], "##[group]Run make lint") {
			t.Errorf("JobLogTail first line = %q, want it to contain %q", lines[0], "##[group]Run make lint")
		}
		if !strings.Contains(lines[len(lines)-1], "##[error]Process completed with exit code 2.") {
			t.Errorf("JobLogTail last line = %q, want it to contain %q", lines[len(lines)-1], "##[error]Process completed with exit code 2.")
		}
		laterStepLeaked := strings.Contains(got, "post-job cleanup") || strings.Contains(got, "removing temp directory") || strings.Contains(got, "unrelated later failure")
		if laterStepLeaked {
			t.Errorf("JobLogTail = %q, must not contain the later step's lines", got)
		}
	})

	t.Run("caps the failed step at n lines, still ending at the last error", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		fmt.Fprintf(&b, "%s ##[group]Run make lint\n", ts(0))
		for i := range 248 {
			fmt.Fprintf(&b, "%s output line %d\n", ts(1+i), i)
		}
		fmt.Fprintf(&b, "%s ##[error]Process completed with exit code 2.\n", ts(249))

		g := serve(t, b.String())

		got, err := g.JobLogTail(t.Context(), "acme", "widgets", 99, 200)
		if err != nil {
			t.Fatalf("JobLogTail: unexpected error: %v", err)
		}

		lines := strings.Split(got, "\n")
		if len(lines) != 200 {
			t.Fatalf("JobLogTail lines = %d, want 200 (%q)", len(lines), got)
		}
		if !strings.Contains(lines[0], "##[group]Run make lint") {
			t.Errorf("JobLogTail first line = %q, want it to contain %q", lines[0], "##[group]Run make lint")
		}
		if !strings.Contains(lines[len(lines)-1], "##[error]Process completed with exit code 2.") {
			t.Errorf("JobLogTail last line = %q, want it to contain %q", lines[len(lines)-1], "##[error]Process completed with exit code 2.")
		}
	})
}

func TestClassifyGitHubErr(t *testing.T) {
	t.Parallel()

	t.Run("401 is ErrGitHubAuth", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message": "Bad credentials"}`)
		})
		g := newTestGHClient(t, mux)

		_, err := g.Viewer(t.Context())
		if !errors.Is(err, ErrGitHubAuth) {
			t.Errorf("error = %v, want errors.Is(err, ErrGitHubAuth)", err)
		}
	})

	t.Run("403 without rate-limit headers is ErrGitHubAuth", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message": "Forbidden"}`)
		})
		g := newTestGHClient(t, mux)

		_, err := g.Viewer(t.Context())
		if !errors.Is(err, ErrGitHubAuth) {
			t.Errorf("error = %v, want errors.Is(err, ErrGitHubAuth)", err)
		}
		if rle, ok := errors.AsType[RateLimitedError](err); ok {
			t.Errorf("error = %v, want it not to classify as RateLimitedError, got %+v", err, rle)
		}
	})

	t.Run("403 with rate-limit headers is RateLimitedError with the reset time", func(t *testing.T) {
		t.Parallel()
		resetAt := time.Now().Add(37 * time.Minute).Truncate(time.Second)
		mux := http.NewServeMux()
		mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message": "API rate limit exceeded"}`)
		})
		g := newTestGHClient(t, mux)

		_, err := g.Viewer(t.Context())
		rle, ok := errors.AsType[RateLimitedError](err)
		if !ok {
			t.Fatalf("error = %v, want errors.As(err, *RateLimitedError)", err)
		}
		if !rle.ResetAt.Equal(resetAt) {
			t.Errorf("ResetAt = %v, want %v", rle.ResetAt, resetAt)
		}
	})

	t.Run("429 is RateLimitedError", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message": "too many requests"}`)
		})
		g := newTestGHClient(t, mux)

		before := time.Now()
		_, err := g.Viewer(t.Context())
		rle, ok := errors.AsType[RateLimitedError](err)
		if !ok {
			t.Fatalf("error = %v, want errors.As(err, *RateLimitedError)", err)
		}
		wantEarliest := before.Add(119 * time.Second)
		wantLatest := time.Now().Add(121 * time.Second)
		if rle.ResetAt.Before(wantEarliest) || rle.ResetAt.After(wantLatest) {
			t.Errorf("ResetAt = %v, want it within [%v, %v]", rle.ResetAt, wantEarliest, wantLatest)
		}
	})

	t.Run("404 is ErrGitHubNotFound", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message": "Not Found"}`)
		})
		g := newTestGHClient(t, mux)

		_, err := g.Viewer(t.Context())
		if !errors.Is(err, ErrGitHubNotFound) {
			t.Errorf("error = %v, want errors.Is(err, ErrGitHubNotFound)", err)
		}
	})

	t.Run("502 is ErrGitHubUnavailable", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"message": "Bad Gateway"}`)
		})
		g := newTestGHClient(t, mux)

		_, err := g.Viewer(t.Context())
		if !errors.Is(err, ErrGitHubUnavailable) {
			t.Errorf("error = %v, want errors.Is(err, ErrGitHubUnavailable)", err)
		}
	})

	t.Run("a network error is ErrGitHubUnavailable", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.NewServeMux())
		server.Close() // refuses every connection from here on

		serverURL := server.URL + "/"
		c, err := github.NewClient(github.WithAuthToken(testGHToken), github.WithURLs(&serverURL, &serverURL))
		if err != nil {
			t.Fatalf("github.NewClient: %v", err)
		}
		g := &GitHubClient{c: c}

		_, err = g.Viewer(t.Context())
		if !errors.Is(err, ErrGitHubUnavailable) {
			t.Errorf("error = %v, want errors.Is(err, ErrGitHubUnavailable)", err)
		}
	})
}
