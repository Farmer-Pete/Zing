package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	zing "zing"
	"zing/internal/lens"
	"zing/internal/machine"
	"zing/internal/response"
	"zing/internal/schemagen"
	"zing/internal/store"
)

// runSelftest proves the foundation on an empty machine: it migrates a fresh
// temporary database and checks it. It prints "selftest: OK" and returns 0
// when every step passes, or prints "selftest: <detail>" for the first
// failure and returns 1.
func runSelftest() int {
	if err := selftest(); err != nil {
		fmt.Fprintf(os.Stderr, "selftest: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, "selftest: OK")
	return 0
}

func selftest() error {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "zing-selftest")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	s, err := store.Open(ctx, filepath.Join(dir, "zing.db"))
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	if err = s.VerifyTables(ctx); err != nil {
		return fmt.Errorf("verify tables: %w", err)
	}

	diffs, err := schemagen.Diff(store.SchemaFS())
	if err != nil {
		return fmt.Errorf("schema drift: %w", err)
	}
	if len(diffs) > 0 {
		return fmt.Errorf("committed schema differs from the generator: %s", diffs[0])
	}

	if _, err = machine.Load(zing.Assets, "machine.toml"); err != nil {
		return err
	}

	lenses, err := lens.Load(zing.Assets, "prompts/lenses")
	if err != nil {
		return err
	}
	if len(lenses) != 8 {
		return fmt.Errorf("expected 8 lenses, found %d", len(lenses))
	}

	if err := s.ValidateExamples(); err != nil {
		return err
	}

	if err := checkResponseTemplates(); err != nil {
		return err
	}

	if err := checkResponseExamples(response.ExampleFS); err != nil {
		return err
	}

	return nil
}

// checkResponseTemplates renders every registered (job, outcome) pair's
// annotated template, failing on the first error (design section 6.10):
// a template exists for exactly the pairs the parser accepts.
// response.RegisteredPairs is internal/response's own single source of
// truth for that enumeration, so this can never drift from what Parse
// actually accepts.
func checkResponseTemplates() error {
	for _, p := range response.RegisteredPairs() {
		if _, err := response.RenderTemplate(p.Job, p.Outcome); err != nil {
			return fmt.Errorf("render %s/%s: %w", p.Job, p.Outcome, err)
		}
	}
	return nil
}

// checkResponseExamples parses and validates every example under fsys's
// examples/ directory (feature kind, no FS), design section 6.10. It reads
// through fsys, rather than response.ExampleFS directly, so a test can
// substitute a tampered filesystem without touching the real embedded
// files.
func checkResponseExamples(fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, "examples")
	if err != nil {
		return fmt.Errorf("list examples: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := "examples/" + e.Name()
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		doc, err := response.Parse(data)
		if err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		if errs := response.Validate(doc, response.ValidateContext{Kind: response.KindFeature}); len(errs) > 0 {
			return fmt.Errorf("validate %s: %w", name, errs[0])
		}
	}
	return nil
}
