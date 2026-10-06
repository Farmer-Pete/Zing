package runtime

import "testing"

// goTestAllCmd is the project's own full-suite test command, named once so
// the table below doesn't repeat the literal past goconst's threshold.
const goTestAllCmd = "go test ./..."

// cdWebNpmTestCmd is a compound deny entry, named once so
// TestDeniedCommand_EntryNotStrippedOrSplit doesn't repeat the literal
// past goconst's threshold.
const cdWebNpmTestCmd = "cd web && npm test"

// TestDeniedCommand exercises the matching rule in shape exactly: split on
// &&, ||, ;, |, and newline, strip a leading time and VAR=value
// assignments from each segment, normalize whitespace, then compare
// against each deny entry for equality or a prefix followed by a space.
func TestDeniedCommand(t *testing.T) {
	t.Parallel()

	deny := []string{goTestAllCmd, denyTestCmd, denyLintCmd}

	matches := []struct {
		name    string
		command string
		want    string
	}{
		{"timed pipe", "time go test ./... | tail -40", goTestAllCmd},
		{"env prefixed", "CGO_ENABLED=0 go test ./... -count=1", goTestAllCmd},
		{"second segment", "cd sub && make test", denyTestCmd},
		{"env then time", "FOO=1 time make lint -v", denyLintCmd},
		{"extra spaces", "go  test   ./...", goTestAllCmd},
		{"semicolon", "cd sub; make test", denyTestCmd},
		{"newline", "cd sub\nmake test", denyTestCmd},
		{"or-or", "false || go test ./...", goTestAllCmd},
	}
	for _, tc := range matches {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry, ok := DeniedCommand(tc.command, deny)
			if !ok {
				t.Fatalf("DeniedCommand(%q) ok = false, want true", tc.command)
			}
			if entry != tc.want {
				t.Errorf("DeniedCommand(%q) entry = %q, want %q", tc.command, entry, tc.want)
			}
		})
	}

	noMatches := []string{
		"make test-short",
		"go test -run X ./a",
		"echo make test",
		"",
		"make test-short || true",
	}
	for _, command := range noMatches {
		t.Run("no match: "+command, func(t *testing.T) {
			t.Parallel()
			entry, ok := DeniedCommand(command, deny)
			if ok {
				t.Errorf("DeniedCommand(%q) = %q, true; want no match", command, entry)
			}
		})
	}
}

// TestDeniedCommand_EntryNotStrippedOrSplit pins the matching rule's own
// treatment of the deny entry itself: it is normalized to single spaces,
// but never stripped of a leading time or VAR=value, and never split on a
// shell operator (#53 r2f5, r2f8). A compound entry such as "cd web &&
// npm test" is matched against the whole normalized command, not against
// one of its segments, so a targeted call through the same shell operator
// still runs.
func TestDeniedCommand_EntryNotStrippedOrSplit(t *testing.T) {
	t.Parallel()

	t.Run("entry's own env prefix is not stripped", func(t *testing.T) {
		t.Parallel()
		deny := []string{"CGO_ENABLED=0 go test ./..."}
		if entry, ok := DeniedCommand("go test ./...", deny); ok {
			t.Errorf("DeniedCommand(%q) = %q, true; want no match", "go test ./...", entry)
		}
	})

	t.Run("compound entry denies the whole command", func(t *testing.T) {
		t.Parallel()
		deny := []string{cdWebNpmTestCmd}
		entry, ok := DeniedCommand(cdWebNpmTestCmd, deny)
		if !ok {
			t.Fatal("DeniedCommand ok = false, want true")
		}
		if entry != deny[0] {
			t.Errorf("entry = %q, want %q", entry, deny[0])
		}
	})

	t.Run("compound entry's own first segment stays allowed", func(t *testing.T) {
		t.Parallel()
		deny := []string{cdWebNpmTestCmd}
		if entry, ok := DeniedCommand("cd web", deny); ok {
			t.Errorf("DeniedCommand(%q) = %q, true; want no match", "cd web", entry)
		}
	})

	t.Run("targeted call through the same operator stays allowed", func(t *testing.T) {
		t.Parallel()
		deny := []string{cdWebNpmTestCmd}
		command := "cd web && npm test -- foo.spec"
		if entry, ok := DeniedCommand(command, deny); ok {
			t.Errorf("DeniedCommand(%q) = %q, true; want no match", command, entry)
		}
	})
}
