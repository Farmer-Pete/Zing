package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"zing/internal/store"
)

// runSelftest proves the foundation on an empty machine: it migrates a fresh
// temporary database and checks it. It prints "selftest: OK" and returns 0
// when every step passes, or prints "selftest: <detail>" for the first
// failure and returns 1.
func runSelftest() int {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "zing-selftest")
	if err != nil {
		fmt.Fprintf(os.Stderr, "selftest: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	s, err := store.Open(ctx, filepath.Join(dir, "zing.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "selftest: %v\n", err)
		return 1
	}
	defer func() { _ = s.Close() }()

	fmt.Fprintln(os.Stdout, "selftest: OK")
	return 0
}
