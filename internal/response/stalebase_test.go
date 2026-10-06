package response

import "testing"

func TestStaleBaseLine(t *testing.T) {
	t.Parallel()

	const sha = "abc1234abc1234abc1234abc1234abc1234abcd"

	tests := []struct {
		reason string
		want   string
	}{
		{"no_origin", "Used the last fetched main at abc1234 for reviewing: origin is missing or is not a git repository."},
		{"remote_ref_missing", "Used the last fetched main at abc1234 for reviewing: origin has no branch main."},
		{"auth", "Used the last fetched main at abc1234 for reviewing: origin refused the credentials."},
		{"network", "Used the last fetched main at abc1234 for reviewing: origin could not be reached."},
		{"fetch_failed", "Used the last fetched main at abc1234 for reviewing: git fetch failed."},
		{"something_unknown", "Used the last fetched main at abc1234 for reviewing: git fetch failed."},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()

			e := StaleBaseEvent{Step: "reviewing", Branch: "main", SHA: sha, Reason: tt.reason}
			got := StaleBaseLine(e)
			if got != tt.want {
				t.Errorf("StaleBaseLine(%+v) = %q, want %q", e, got, tt.want)
			}
		})
	}
}
