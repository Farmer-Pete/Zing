package response

import (
	"bytes"
	"encoding/json"
	"testing"
)

// mustJSON marshals v for a round-trip byte comparison, per this package's
// standard-library-only test convention (no testify, no go-cmp).
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func TestFiles_EmptyPlanReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := Files(Plan{}); len(got) != 0 {
		t.Errorf("Files(Plan{}) = %v, want empty", got)
	}
}

func TestFences_EmptyPlanReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := Fences(Plan{}); len(got) != 0 {
		t.Errorf("Fences(Plan{}) = %v, want empty", got)
	}
}

func TestTasks_EmptyPlanReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := Tasks(Plan{}); len(got) != 0 {
		t.Errorf("Tasks(Plan{}) = %v, want empty", got)
	}
}

func TestFiles_ReturnsDeliveryFiles(t *testing.T) {
	t.Parallel()
	p := cleanPlan()
	got, want := Files(p), p.Delivery.Files
	if !bytes.Equal(mustJSON(t, got), mustJSON(t, want)) {
		t.Errorf("Files(p) = %s, want %s", mustJSON(t, got), mustJSON(t, want))
	}
}

// TestFences_ReturnsDeletionItems proves Fences reads
// p.Delivery.Deletions.Items, hiding the $.delivery.deletions.deletions
// nesting design section 6.11 calls out.
func TestFences_ReturnsDeletionItems(t *testing.T) {
	t.Parallel()
	p := cleanPlan()
	p.Delivery.Deletions = Deletions{
		Items: []Fence{{Path: testMainGo, Symbol: "oldHandler", ExistedBecause: "existed because it predated the rewrite"}},
	}
	got, want := Fences(p), p.Delivery.Deletions.Items
	if !bytes.Equal(mustJSON(t, got), mustJSON(t, want)) {
		t.Errorf("Fences(p) = %s, want %s", mustJSON(t, got), mustJSON(t, want))
	}
}

func TestTasks_ReturnsDeliveryTasks(t *testing.T) {
	t.Parallel()
	p := cleanPlan()
	got, want := Tasks(p), p.Delivery.Tasks
	if !bytes.Equal(mustJSON(t, got), mustJSON(t, want)) {
		t.Errorf("Tasks(p) = %s, want %s", mustJSON(t, got), mustJSON(t, want))
	}
}
