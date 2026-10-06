package runtime

import "testing"

// goTestAllCmd is the project's own full-suite test command, named once so
// the table below doesn't repeat the literal past goconst's threshold.
const goTestAllCmd = "go test ./..."

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

// TestDeniedCommand_EntryItselfNeedsNormalizing proves a deny entry that
// carries an env assignment or is itself a compound command still matches
// the bare segment a build run tries to run (the project's own configured
// test or lint command, which this hook exists to block, often looks
// exactly like this).
func TestDeniedCommand_EntryItselfNeedsNormalizing(t *testing.T) {
	t.Parallel()

	t.Run("entry has a leading env assignment", func(t *testing.T) {
		t.Parallel()
		deny := []string{"CGO_ENABLED=0 go test ./..."}
		entry, ok := DeniedCommand("CGO_ENABLED=0 go test ./...", deny)
		if !ok {
			t.Fatal("DeniedCommand ok = false, want true")
		}
		if entry != deny[0] {
			t.Errorf("entry = %q, want %q", entry, deny[0])
		}
	})

	t.Run("entry is a compound command", func(t *testing.T) {
		t.Parallel()
		deny := []string{"go vet ./... && go test ./..."}
		entry, ok := DeniedCommand("go test ./...", deny)
		if !ok {
			t.Fatal("DeniedCommand ok = false, want true")
		}
		if entry != deny[0] {
			t.Errorf("entry = %q, want %q", entry, deny[0])
		}
	})
}
