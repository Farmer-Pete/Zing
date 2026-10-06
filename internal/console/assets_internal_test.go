package console

import (
	"bytes"
	"regexp"
	"testing"
)

// assetVersionPattern is what hashAssets and assetVersion must always
// match: the first assetVersionLen hex characters of a sha256 (#59, Q2
// decision).
var assetVersionPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// TestHashAssets proves hashAssets is a stable, content-sensitive digest of
// the five embedded assets (#59, Q2 decision): changing one byte of one
// asset changes the version, the same five bytes always hash to the same
// version, and assetVersion itself is exactly that hash of the real
// embedded bytes.
func TestHashAssets(t *testing.T) {
	flipped := bytes.Clone(consoleJS)
	flipped[0] ^= 0x01

	got := hashAssets(datastarJS, mermaidJS, flipped, keyboardMJS, keysJSON)
	if got == assetVersion {
		t.Errorf("hashAssets with a flipped byte = %q, want different from assetVersion %q", got, assetVersion)
	}
	if !assetVersionPattern.MatchString(got) {
		t.Errorf("hashAssets = %q, want to match %s", got, assetVersionPattern)
	}
	if again := hashAssets(datastarJS, mermaidJS, flipped, keyboardMJS, keysJSON); again != got {
		t.Errorf("hashAssets is not stable: got %q then %q for the same input", got, again)
	}

	if want := hashAssets(datastarJS, mermaidJS, consoleJS, keyboardMJS, keysJSON); assetVersion != want {
		t.Errorf("assetVersion = %q, want %q (hashAssets of the real embedded bytes)", assetVersion, want)
	}
}
