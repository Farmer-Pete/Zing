package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// decodeGraphQLBody decodes r's JSON body's "variables" into a map, failing
// the test (not the request) on a decode error: the httptest server invokes
// handlers on their own goroutine, so a failure must be recorded with
// t.Errorf, never t.Fatalf (github_test.go's TestGHClientCreateDraftPR
// explains why).
func decodeGraphQLBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode graphql request body: %v", err)
		return nil
	}
	return body.Variables
}

// stringVar reads vars[key] as a string, or "" when it is absent or JSON
// null: every "after" cursor these tests read is null on the first page.
func stringVar(vars map[string]any, key string) string {
	s, ok := vars[key].(string)
	if !ok {
		return ""
	}
	return s
}

func TestListThreadsPages(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		vars := decodeGraphQLBody(t, r)
		after := stringVar(vars, "after")
		switch after {
		case "":
			fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{
				"pageInfo": {"hasNextPage": true, "endCursor": "c1"},
				"nodes": [{"id": "T1", "isResolved": false, "isOutdated": false, "path": "a.go", "line": null, "comments": {"nodes": []}}]
			}}}}}`)
		case "c1":
			fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{
				"pageInfo": {"hasNextPage": false, "endCursor": ""},
				"nodes": [{"id": "T2", "isResolved": true, "isOutdated": true, "path": "b.go", "line": 12, "comments": {"nodes": []}}]
			}}}}}`)
		default:
			t.Errorf("unexpected after %q", after)
		}
	})

	g := newTestGHClient(t, mux)

	got, err := g.ListThreads(t.Context(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("ListThreads: unexpected error: %v", err)
	}
	want := []Thread{
		{ID: "T1", IsResolved: false, IsOutdated: false, Path: "a.go", Line: 0, Comments: []ThreadComment{}},
		{ID: "T2", IsResolved: true, IsOutdated: true, Path: "b.go", Line: 12, Comments: []ThreadComment{}},
	}
	if len(got) != len(want) {
		t.Fatalf("ListThreads = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].IsResolved != want[i].IsResolved || got[i].IsOutdated != want[i].IsOutdated ||
			got[i].Path != want[i].Path || got[i].Line != want[i].Line {
			t.Errorf("ListThreads[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestListThreadsTooMany(t *testing.T) {
	t.Parallel()

	var requests int
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		requests++
		cursor := fmt.Sprintf("c%d", requests)
		fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{
			"pageInfo": {"hasNextPage": true, "endCursor": %q},
			"nodes": [{"id": %q, "isResolved": false, "isOutdated": false, "path": "a.go", "line": 1, "comments": {"nodes": []}}]
		}}}}}`, cursor, cursor)
	})

	g := newTestGHClient(t, mux)

	_, err := g.ListThreads(t.Context(), "acme", "widgets", 7)
	if err == nil {
		t.Fatal("ListThreads: expected an error, got nil")
	}
	want := "orchestrator: graphql threads: more than 1000 review threads"
	if err.Error() != want {
		t.Errorf("ListThreads error = %q, want %q", err.Error(), want)
	}
	if requests != 10 {
		t.Errorf("requests made = %d, want 10 (no eleventh page fetched)", requests)
	}
}

func TestReplyToThread(t *testing.T) {
	t.Parallel()

	var gotVars map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		gotVars = decodeGraphQLBody(t, r)
		fmt.Fprint(w, `{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":"C1"}}}}`)
	})

	g := newTestGHClient(t, mux)

	const body = "Zing (an AI agent) replying: thanks, fixed."
	if err := g.ReplyToThread(t.Context(), "T1", body); err != nil {
		t.Fatalf("ReplyToThread: unexpected error: %v", err)
	}
	want := map[string]any{"thread": "T1", "body": body}
	if len(gotVars) != len(want) || gotVars["thread"] != want["thread"] || gotVars["body"] != want["body"] {
		t.Errorf("variables = %#v, want %#v", gotVars, want)
	}
}

// fillerThreadComments builds n "nodes" entries of a ZingThreadComments
// page, in raw JSON text: the first is authored by author and holds
// needle's exact text (TestThreadCommentsContainPagesAll's "first of 250
// comments"); the rest are unrelated filler from another author.
func fillerThreadComments(n int, needle, author string) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		body, login := "an unrelated comment", "alice"
		if i == 0 {
			body, login = needle, author
		}
		fmt.Fprintf(&b, `{"body": %q, "author": {"login": %q}}`, body, login)
	}
	return b.String()
}

func TestThreadCommentsContainPagesAll(t *testing.T) {
	t.Parallel()

	const needle = "<!-- zing:reply a1 t1 -->"
	const author = "zing-bot"

	var requests int
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		requests++
		vars := decodeGraphQLBody(t, r)
		after := stringVar(vars, "after")
		if after != "" {
			t.Errorf("ThreadCommentsContain fetched a page after finding the marker on the first page (after=%q)", after)
		}
		// 250 comments total across three pages; the marker is the very
		// first comment, so ThreadCommentsContain must find it without
		// paging past this first page of 100.
		fmt.Fprintf(w, `{"data":{"node":{"comments":{
			"pageInfo": {"hasNextPage": true, "endCursor": "c1"},
			"nodes": [%s]
		}}}}`, fillerThreadComments(100, needle, author))
	})

	g := newTestGHClient(t, mux)

	found, err := g.ThreadCommentsContain(t.Context(), "T1", needle, author)
	if err != nil {
		t.Fatalf("ThreadCommentsContain: unexpected error: %v", err)
	}
	if !found {
		t.Error("ThreadCommentsContain = false, want true")
	}
	if requests != 1 {
		t.Errorf("requests made = %d, want 1", requests)
	}
}

func TestThreadCommentsContainIgnoresOtherAuthors(t *testing.T) {
	t.Parallel()

	const needle = "<!-- zing:reply a1 t1 -->"

	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":{"node":{"comments":{
			"pageInfo": {"hasNextPage": false, "endCursor": ""},
			"nodes": [{"body": %q, "author": {"login": "a-stranger"}}]
		}}}}`, needle)
	})

	g := newTestGHClient(t, mux)

	found, err := g.ThreadCommentsContain(t.Context(), "T1", needle, "zing-bot")
	if err != nil {
		t.Fatalf("ThreadCommentsContain: unexpected error: %v", err)
	}
	if found {
		t.Error("ThreadCommentsContain = true, want false: the matching comment is not authored by zing-bot")
	}
}

func TestResolveThreadChecksResult(t *testing.T) {
	t.Parallel()

	t.Run("isResolved true succeeds", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"resolveReviewThread":{"thread":{"id":"T1","isResolved":true}}}}`)
		})
		g := newTestGHClient(t, mux)
		if err := g.ResolveThread(t.Context(), "T1"); err != nil {
			t.Fatalf("ResolveThread: unexpected error: %v", err)
		}
	})

	t.Run("isResolved false is an error", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"resolveReviewThread":{"thread":{"id":"T1","isResolved":false}}}}`)
		})
		g := newTestGHClient(t, mux)
		err := g.ResolveThread(t.Context(), "T1")
		want := "orchestrator: graphql resolve: result did not change state"
		if err == nil || err.Error() != want {
			t.Errorf("ResolveThread error = %v, want %q", err, want)
		}
	})
}

func TestMarkReadyChecksResult(t *testing.T) {
	t.Parallel()

	t.Run("isDraft false succeeds", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"isDraft":false}}}}`)
		})
		g := newTestGHClient(t, mux)
		if err := g.MarkReady(t.Context(), "PR1"); err != nil {
			t.Fatalf("MarkReady: unexpected error: %v", err)
		}
	})

	t.Run("isDraft true is an error", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"isDraft":true}}}}`)
		})
		g := newTestGHClient(t, mux)
		err := g.MarkReady(t.Context(), "PR1")
		want := "orchestrator: graphql ready: result did not change state"
		if err == nil || err.Error() != want {
			t.Errorf("MarkReady error = %v, want %q", err, want)
		}
	})
}

func TestConvertToDraftChecksResult(t *testing.T) {
	t.Parallel()

	t.Run("isDraft true succeeds", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"convertPullRequestToDraft":{"pullRequest":{"isDraft":true}}}}`)
		})
		g := newTestGHClient(t, mux)
		if err := g.ConvertToDraft(t.Context(), "PR1"); err != nil {
			t.Fatalf("ConvertToDraft: unexpected error: %v", err)
		}
	})

	t.Run("isDraft false is an error", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"convertPullRequestToDraft":{"pullRequest":{"isDraft":false}}}}`)
		})
		g := newTestGHClient(t, mux)
		err := g.ConvertToDraft(t.Context(), "PR1")
		want := "orchestrator: graphql draft: result did not change state"
		if err == nil || err.Error() != want {
			t.Errorf("ConvertToDraft error = %v, want %q", err, want)
		}
	})
}

func TestGraphQLErrorNotFound(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a node with the global id of 'bogus'."}]}`)
	})

	g := newTestGHClient(t, mux)

	err := g.ResolveThread(t.Context(), "bogus")
	if !errors.Is(err, ErrGitHubNotFound) {
		t.Errorf("ResolveThread error = %v, want errors.Is(err, ErrGitHubNotFound)", err)
	}
}

func TestGraphQLErrorMessage(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`)
	})

	g := newTestGHClient(t, mux)

	err := g.ResolveThread(t.Context(), "T1")
	want := "orchestrator: graphql resolve: Resource not accessible by integration"
	if err == nil || err.Error() != want {
		t.Errorf("ResolveThread error = %v, want %q", err, want)
	}
}

func TestGraphQLUsesClientToken(t *testing.T) {
	t.Parallel()

	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"data":{"resolveReviewThread":{"thread":{"id":"T1","isResolved":true}}}}`)
	})

	g := newTestGHClient(t, mux)

	if err := g.ResolveThread(t.Context(), "T1"); err != nil {
		t.Fatalf("ResolveThread: unexpected error: %v", err)
	}
	if want := "Bearer " + testGHToken; gotAuth != want {
		t.Errorf("Authorization header = %q, want %q", gotAuth, want)
	}
}
