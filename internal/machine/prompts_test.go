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
			sha256: "b60306b3d973a61c36734749c7e5fda7bad75125d25f5c2e0d165c1131c1b6fc",
		},
		{
			name:   "planning-bug",
			path:   "prompts/planning-bug.md",
			sha256: "771b5a0e2194969ac66ebea40acfad9c102de8207bef5d2f63ced1567a25cb1b",
		},
		{
			name:   "planreview",
			path:   "prompts/planreview.md",
			sha256: "dfcb9f616c3520bca65330d4b8db6abf85c1476bc0afd1a765f868ae5d68eb9b",
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
