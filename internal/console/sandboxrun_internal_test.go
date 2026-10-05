package console

import "testing"

// TestIsLoopbackRemote proves isLoopbackRemote accepts only 127.0.0.1 and
// ::1 (IPv4-mapped forms included) and rejects every other shape, including
// other 127.x addresses and malformed input.
func TestIsLoopbackRemote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		remoteAddr string
		want       bool
	}{
		{"127.0.0.1:5", true},
		{"[::1]:5", true},
		{"[::ffff:127.0.0.1]:5", true},
		{"127.0.0.2:5", false},
		{"192.0.2.1:5", false},
		{"[fe80::1]:5", false},
		{"garbage", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.remoteAddr, func(t *testing.T) {
			t.Parallel()
			if got := isLoopbackRemote(tc.remoteAddr); got != tc.want {
				t.Errorf("isLoopbackRemote(%q) = %v, want %v", tc.remoteAddr, got, tc.want)
			}
		})
	}
}
