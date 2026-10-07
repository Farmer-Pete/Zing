package dispatch

import (
	"testing"
	"testing/fstest"

	"zing/internal/tracker"
)

// TestIssueTextOwnerCommentsOnly proves issueText filters comments to the
// binding's own User (#98): a pickup comment Zing itself would have posted
// is dropped, and a comment by a different author never reaches the result.
func TestIssueTextOwnerCommentsOnly(t *testing.T) {
	fsys := fstest.MapFS{"tickets.toml": &fstest.MapFile{Data: []byte(`project = "zing"

[[ticket]]
ref = "fake#1"
title = "one"
body = "body one"
`)}}
	fx, err := tracker.NewFixture(fsys, "tickets.toml")
	if err != nil {
		t.Fatalf("tracker.NewFixture: %v", err)
	}

	ctx := t.Context()
	if commentErr := fx.Comment(ctx, "zing", "fake#1", tracker.PickupComment("fixture-user")); commentErr != nil {
		t.Fatalf("Comment (pickup): %v", commentErr)
	}
	if commentErr := fx.Comment(ctx, "zing", "fake#1", "Use serve."); commentErr != nil {
		t.Fatalf("Comment: %v", commentErr)
	}

	b := Binding{TrackerProject: "zing", User: "fixture-user"}
	it, err := issueText(ctx, fx, b, "fake#1")
	if err != nil {
		t.Fatalf("issueText: %v", err)
	}
	if it.Body != "body one" {
		t.Errorf("Body = %q, want %q", it.Body, "body one")
	}
	if it.CommentCount != 1 {
		t.Errorf("CommentCount = %d, want 1", it.CommentCount)
	}
	wantComments := "Comment by fixture-user:\nUse serve."
	if it.OwnerComments != wantComments {
		t.Errorf("OwnerComments = %q, want %q", it.OwnerComments, wantComments)
	}

	other := Binding{TrackerProject: "zing", User: "someone-else"}
	it2, err := issueText(ctx, fx, other, "fake#1")
	if err != nil {
		t.Fatalf("issueText (other user): %v", err)
	}
	if it2.CommentCount != 0 {
		t.Errorf("CommentCount (other user) = %d, want 0", it2.CommentCount)
	}
}
