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

	deny := []string{goTestAllCmd, "make test", "make lint"}

	matches := []struct {
		name    string
		command string
		want    string
	}{
		{"timed pipe", "time go test ./... | tail -40", goTestAllCmd},
		{"env prefixed", "CGO_ENABLED=0 go test ./... -count=1", goTestAllCmd},
		{"second segment", "cd sub && make test", "make test"},
		{"env then time", "FOO=1 time make lint -v", "make lint"},
		{"extra spaces", "go  test   ./...", goTestAllCmd},
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
