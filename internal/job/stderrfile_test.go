package job

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteStderrFile proves a run's stderr lands in a private file under
// the data dir, so a failed run can be diagnosed without logging the text.
func TestWriteStderrFile(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	path, err := writeStderrFile(dataDir, 41, []byte("Error: sandbox denied\n"))
	if err != nil {
		t.Fatalf("writeStderrFile: %v", err)
	}
	if want := filepath.Join(dataDir, "runs", "run-41-stderr.log"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "Error: sandbox denied\n" {
		t.Errorf("file = %q, %v, want the stderr text", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v, want 0600", info.Mode().Perm(), err)
	}
}
