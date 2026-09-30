package greeter

import "testing"

func TestGreet(t *testing.T) {
	if got, want := Greet("Ada"), "Hello, Ada!"; got != want {
		t.Errorf("Greet(%q) = %q, want %q", "Ada", got, want)
	}
}
