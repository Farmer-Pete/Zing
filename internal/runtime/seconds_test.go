package runtime

import (
	"testing"
	"time"
)

func TestSeconds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		d    time.Duration
		want int
	}{
		{"zero", 0, 1},
		{"under a second", 1200 * time.Millisecond, 2},
		{"exactly two seconds", 2 * time.Second, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := Seconds(c.d); got != c.want {
				t.Errorf("Seconds(%v) = %d, want %d", c.d, got, c.want)
			}
		})
	}
}
