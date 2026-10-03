package job

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTailBufferKeepsLastBytes proves a tailBuffer written in uneven chunks
// keeps exactly the last checkOutputCap bytes and counts every byte, and
// that a short write comes back whole.
func TestTailBufferKeepsLastBytes(t *testing.T) {
	t.Parallel()
	input := make([]byte, 40960)
	for i := range input {
		input[i] = 'a' + byte(i%26)
	}
	b := newTailBuffer(checkOutputCap)
	rest := input
	for _, n := range []int{1, 5000, 17000, 3, 9000, 6000} {
		if _, err := b.Write(rest[:n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		rest = rest[n:]
	}
	if _, err := b.Write(rest); err != nil { // the seventh chunk
		t.Fatalf("Write: %v", err)
	}
	if got, want := b.Tail(), string(input[len(input)-checkOutputCap:]); got != want {
		t.Errorf("Tail() is %d bytes, want the last %d bytes of the input", len(got), len(want))
	}
	if b.Total() != 40960 {
		t.Errorf("Total() = %d, want 40960", b.Total())
	}

	short := newTailBuffer(checkOutputCap)
	small := strings.Repeat("z", 100)
	if _, err := short.Write([]byte(small)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if short.Tail() != small {
		t.Errorf("Tail() = %q, want the 100-byte input", short.Tail())
	}
}

// TestTailBufferBoundedOnLargeSingleWrite proves one write far larger than
// the limit never makes the buffer hold more than the limit (review
// finding 3): the retained bytes stay bounded while writing, not only
// after a later trim.
func TestTailBufferBoundedOnLargeSingleWrite(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	big = append(big, "END"...)
	b := newTailBuffer(checkOutputCap)
	n, err := b.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(big))
	}
	if cap(b.buf) > checkOutputCap {
		t.Errorf("cap(buf) = %d after one large write, want <= %d", cap(b.buf), checkOutputCap)
	}
	if got := b.Tail(); got != string(big[len(big)-checkOutputCap:]) {
		t.Errorf("Tail() is not the last %d bytes of the write", checkOutputCap)
	}
	if b.Total() != int64(len(big)) {
		t.Errorf("Total() = %d, want %d", b.Total(), len(big))
	}
}

// TestTailBufferCutsOnRuneBoundary proves a cut that lands inside a UTF-8
// rune drops the rune's leftover continuation bytes.
func TestTailBufferCutsOnRuneBoundary(t *testing.T) {
	t.Parallel()
	b := newTailBuffer(4)
	if _, err := b.Write([]byte("aé€")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := b.Tail()
	if got != "€" {
		t.Errorf("Tail() = %q, want %q", got, "€")
	}
	if !utf8.ValidString(got) {
		t.Errorf("Tail() %q is not valid UTF-8", got)
	}
}
