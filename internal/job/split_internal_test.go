package job

import (
	"strings"
	"testing"

	"zing/internal/response"
)

func child(key string, dependsOn ...string) response.Child {
	return response.Child{Key: key, Title: key, Body: key, DependsOn: dependsOn}
}

// TestSplitOrder proves splitOrder returns children in Kahn's-algorithm
// dependency order, always taking the earliest-listed ready child, and
// reports an unknown dependency key or a cycle as an error (plan #74).
func TestSplitOrder(t *testing.T) {
	t.Parallel()

	t.Run("already in order", func(t *testing.T) {
		t.Parallel()
		got, err := splitOrder([]response.Child{child("c1"), child("c2")})
		if err != nil {
			t.Fatalf("splitOrder: %v", err)
		}
		wantKeys(t, got, "c1", "c2")
	})

	t.Run("dependency reorders", func(t *testing.T) {
		t.Parallel()
		got, err := splitOrder([]response.Child{
			child("c1"),
			child("c2", "c3"),
			child("c3"),
		})
		if err != nil {
			t.Fatalf("splitOrder: %v", err)
		}
		wantKeys(t, got, "c1", "c3", "c2")
	})

	t.Run("unknown dependency key", func(t *testing.T) {
		t.Parallel()
		_, err := splitOrder([]response.Child{child("c1", "c9")})
		if err == nil || !strings.Contains(err.Error(), "depends on unknown key") {
			t.Fatalf("splitOrder: got %v, want error containing %q", err, "depends on unknown key")
		}
	})

	t.Run("cycle", func(t *testing.T) {
		t.Parallel()
		_, err := splitOrder([]response.Child{
			child("c1", "c2"),
			child("c2", "c1"),
		})
		if err == nil || !strings.Contains(err.Error(), "dependency cycle among c1, c2") {
			t.Fatalf("splitOrder: got %v, want error containing %q", err, "dependency cycle among c1, c2")
		}
	})
}

func wantKeys(t *testing.T, got []response.Child, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("splitOrder: got %d children, want %d", len(got), len(want))
	}
	for i, c := range got {
		if c.Key != want[i] {
			t.Fatalf("splitOrder: got keys %v, want %v", keysOf(got), want)
		}
	}
}

func keysOf(cs []response.Child) []string {
	keys := make([]string, len(cs))
	for i, c := range cs {
		keys[i] = c.Key
	}
	return keys
}
