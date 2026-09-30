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
		{
			name:   testJobNameBuild,
			path:   "prompts/build.md",
			sha256: "3a88533d216936fba660e84fcf4dea0004f61442b9195b6ee00b3c2428b10fc8",
		},
		{
			name:   "perimeter",
			path:   "prompts/perimeter.md",
			sha256: "78b4863acf309085bc37f8f33cfea8f430d08b57d44102023eea1f3c03ef00b6",
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

// TestBuildPromptPlaceholders checks that prompts/build.md carries each of
// its five placeholders exactly once, so ForBuild's single replacement of
// each cannot silently miss or double up.
func TestBuildPromptPlaceholders(t *testing.T) {
	t.Parallel()

	got, err := zing.Assets.ReadFile("prompts/build.md")
	if err != nil {
		t.Fatalf("ReadFile(prompts/build.md): %v", err)
	}
	text := string(got)

	placeholders := []string{"{n}", "{total}", "{task title}", "{test_cmd}", "{lint_cmd}"}
	for _, p := range placeholders {
		if n := strings.Count(text, p); n != 1 {
			t.Errorf("prompts/build.md contains %s %d times, want 1", p, n)
		}
	}
}
