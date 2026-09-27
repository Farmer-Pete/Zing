package machine

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	zing "zing"
)

// TestPrompts_MatchThePinnedDesignText pins each Package 7 prompt file's
// SHA-256 to the section 22 design text. A stub regression, a partial
// paste, or an unreviewed edit changes the digest and fails this test.
func TestPrompts_MatchThePinnedDesignText(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		path   string
		sha256 string
	}{
		{
			name:   "classify",
			path:   "prompts/classify.md",
			sha256: "5f20c69c8b34f89171ba2bcc5cbdc836f4865e5ad3039a310db14d6d77800462",
		},
		{
			name:   "planning-feature",
			path:   "prompts/planning-feature.md",
			sha256: "fa7c4e234296affb0efbce560bb0557524d22413fa9d2d37043c4d271ed33698",
		},
		{
			name:   "planning-bug",
			path:   "prompts/planning-bug.md",
			sha256: "541217a029c67cd28c06c956c8829e9455e034d2bbda50077d5cd7ca5d8f78c2",
		},
		{
			name:   "planreview",
			path:   "prompts/planreview.md",
			sha256: "c0cc39368632ea2ffa0f4672e8c576d7d044b26a4cae9c43d7fc81c1c2320d7f",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zing.Assets.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", tc.path, err)
			}

			if strings.Contains(string(got), "Stub prompt") {
				t.Errorf("%s still contains the stub placeholder text", tc.path)
			}

			sum := sha256.Sum256(got)
			if gotHex := hex.EncodeToString(sum[:]); gotHex != tc.sha256 {
				t.Errorf("%s sha256 = %s, want %s", tc.path, gotHex, tc.sha256)
			}
		})
	}
}
