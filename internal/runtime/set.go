package runtime

import (
	"errors"
	"fmt"
	"reflect"
)

// Set maps a job's runtime name -- "claude", "codex", or "fake", the values
// machine.toml's job.runtime field allows -- to the Runtime that serves it
// (design section 4.1, D2). Production wires the two real runtimes under
// "claude" and "codex"; selftest and e2e map all three names to one Fake.
type Set struct {
	byName map[string]Runtime
}

// NewSet returns a Set holding a private clone of m, so a caller that goes
// on to mutate its own map after NewSet returns cannot reach the Set it
// already built. It rejects an empty name, a nil Runtime, and a typed nil
// (a nil pointer, map, chan, func, or slice boxed in the Runtime
// interface): any of those would either panic or silently no-op the first
// time a handler's Run call reached it.
func NewSet(m map[string]Runtime) (Set, error) {
	byName := make(map[string]Runtime, len(m))
	for name, rt := range m {
		if name == "" {
			return Set{}, errors.New("runtime: NewSet: a runtime name must not be empty")
		}
		if rt == nil {
			return Set{}, fmt.Errorf("runtime: NewSet: %s: Runtime must not be nil", name)
		}
		if isTypedNil(rt) {
			return Set{}, fmt.Errorf("runtime: NewSet: %s: Runtime holds a typed nil value", name)
		}
		byName[name] = rt
	}
	return Set{byName: byName}, nil
}

// isTypedNil reports whether rt holds a nil pointer, map, chan, func, or
// slice: an interface value that is not itself == nil (a bare nil Runtime
// is caught by NewSet's own == nil check above) but whose underlying value
// is, and would panic the first time a real method call reached it.
func isTypedNil(rt Runtime) bool {
	v := reflect.ValueOf(rt)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// For returns the Runtime registered under name, or an error naming the
// unknown runtime.
func (s Set) For(name string) (Runtime, error) {
	rt, ok := s.byName[name]
	if !ok {
		return nil, fmt.Errorf("runtime: unknown runtime %q", name)
	}
	return rt, nil
}
