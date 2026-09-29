// greet_test.go is fixture content the fake build runtime writes into a
// ticket worktree that, by the time task 3 runs for real, already carries
// task 2's greet.go from its own landed commit (the tree effect lands on
// top of the checked-out branch, not read as an isolated package). Embedded
// on its own disk path, this file must still be valid, self-contained Go:
// go vet and go test see every fixture ".tree" directory as its own
// package, with no sibling directory's files in scope, so it redeclares a
// local greet rather than referencing greet.go's exported Greet.
package greet

import "testing"

func greet() string { return "hello, world" }

func TestGreet(t *testing.T) {
	if got := greet(); got != "hello, world" {
		t.Errorf("greet() = %q, want %q", got, "hello, world")
	}
}
