package runtime

import (
	"testing"
	"testing/fstest"
)

// testRuntimeClaude, testRuntimeCodex, and testRuntimeFake are the runtime
// names this file's tests register under, pulled out as constants so
// goconst does not flag the repeats.
const (
	testRuntimeClaude = "claude"
	testRuntimeCodex  = "codex"
	testRuntimeFake   = "fake"
)

// TestNewSet_ClonesTheInputMap proves NewSet copies m rather than aliasing
// it: mutating the caller's map after NewSet returns must not reach the
// returned Set (design section 4.1).
func TestNewSet_ClonesTheInputMap(t *testing.T) {
	t.Parallel()

	rt := NewFake(fstest.MapFS{})
	m := map[string]Runtime{testRuntimeClaude: rt}

	set, err := NewSet(m)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}

	m[testRuntimeCodex] = rt // mutate the caller's map after NewSet returns

	if _, err := set.For(testRuntimeCodex); err == nil {
		t.Error(`For("codex") = nil error, want an error: the Set must not see a mutation of the input map made after NewSet returned`)
	}
	if _, err := set.For(testRuntimeClaude); err != nil {
		t.Errorf(`For("claude") = %v, want nil (the original entry must still be there)`, err)
	}
}

// TestNewSet_RejectsAnEmptyName proves an empty runtime name is refused,
// since a handler could never look it up by name.
func TestNewSet_RejectsAnEmptyName(t *testing.T) {
	t.Parallel()

	rt := NewFake(fstest.MapFS{})
	if _, err := NewSet(map[string]Runtime{"": rt}); err == nil {
		t.Error("NewSet(empty name) = nil error, want an error")
	}
}

// TestNewSet_RejectsANilRuntime proves a bare nil interface value is
// refused: calling Run on it would panic the first time a handler reached
// it.
func TestNewSet_RejectsANilRuntime(t *testing.T) {
	t.Parallel()

	if _, err := NewSet(map[string]Runtime{testRuntimeClaude: nil}); err == nil {
		t.Error("NewSet(nil Runtime) = nil error, want an error")
	}
}

// TestNewSet_RejectsATypedNilRuntime proves a typed nil -- a nil *Fake
// boxed in the Runtime interface, which is not == nil itself -- is refused
// too: reflect must catch what a plain == nil check cannot.
func TestNewSet_RejectsATypedNilRuntime(t *testing.T) {
	t.Parallel()

	var nilFake *Fake
	if _, err := NewSet(map[string]Runtime{testRuntimeClaude: nilFake}); err == nil {
		t.Error("NewSet(typed nil *Fake): want an error, got nil")
	}
}

// TestSet_For_UnknownNameErrors proves For names the unknown runtime in its
// error rather than returning some silent zero value.
func TestSet_For_UnknownNameErrors(t *testing.T) {
	t.Parallel()

	set, err := NewSet(map[string]Runtime{testRuntimeClaude: NewFake(fstest.MapFS{})})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}

	_, err = set.For(testRuntimeCodex)
	if err == nil {
		t.Fatal(`For(codex): want an error, got nil`)
	}
	if got := err.Error(); got == "" {
		t.Error("For(unknown): error text is empty, want it to name the runtime")
	}
}

// TestSet_For_HappyPath proves a name registered by NewSet resolves to the
// exact Runtime value it was given.
func TestSet_For_HappyPath(t *testing.T) {
	t.Parallel()

	claude := NewFake(fstest.MapFS{})
	codex := NewFake(fstest.MapFS{})
	set, err := NewSet(map[string]Runtime{testRuntimeClaude: claude, testRuntimeCodex: codex, testRuntimeFake: claude})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}

	got, err := set.For(testRuntimeClaude)
	if err != nil {
		t.Fatalf(`For("claude"): %v`, err)
	}
	if got != Runtime(claude) {
		t.Errorf(`For("claude") = %v, want the exact registered claude Runtime`, got)
	}

	got, err = set.For(testRuntimeCodex)
	if err != nil {
		t.Fatalf(`For("codex"): %v`, err)
	}
	if got != Runtime(codex) {
		t.Errorf(`For("codex") = %v, want the exact registered codex Runtime`, got)
	}
}
