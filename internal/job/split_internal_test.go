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

// TestSplitChildBody proves splitChildBody's rendering rule (design section
// 6.6's split variant, owner decision Q4): body trimmed, then the shared
// notes under their own heading when notes is non-blank, then "Split from"
// the parent's human ref, then "Depends on" the dep refs when there are
// any, each part separated by a blank line.
func TestSplitChildBody(t *testing.T) {
	t.Parallel()

	t.Run("notes and two dependencies", func(t *testing.T) {
		t.Parallel()
		got := splitChildBody("Do X", "N", "65", []string{"70", "71"})
		want := "Do X\n\n## Shared notes from the split\n\nN\n\nSplit from #65.\n\nDepends on #70, #71."
		if got != want {
			t.Errorf("splitChildBody = %q, want %q", got, want)
		}
	})

	t.Run("blank notes leave out the heading", func(t *testing.T) {
		t.Parallel()
		got := splitChildBody("Detect the conflict", "", "65", []string{"70"})
		if strings.Contains(got, "Shared notes") {
			t.Errorf("splitChildBody = %q, want no Shared notes heading", got)
		}
	})

	t.Run("no dependencies leave out the Depends on line", func(t *testing.T) {
		t.Parallel()
		got := splitChildBody("Build the merge unit", "", "65", nil)
		want := "Build the merge unit\n\nSplit from #65."
		if got != want {
			t.Errorf("splitChildBody = %q, want %q", got, want)
		}
		if strings.Contains(got, "Depends on") {
			t.Errorf("splitChildBody = %q, want no Depends on line", got)
		}
	})

	t.Run("non-numeric parent ref renders unchanged", func(t *testing.T) {
		t.Parallel()
		got := splitChildBody("Merge earlier", "", "fake#3", nil)
		want := "Merge earlier\n\nSplit from fake#3."
		if got != want {
			t.Errorf("splitChildBody = %q, want %q", got, want)
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
