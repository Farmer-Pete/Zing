package fixtures_test

import (
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"

	"zing/fixtures"
	"zing/internal/response"
)

// repoRootFS is the real repository filesystem, rooted one level up from
// this package's directory (fixtures/ sits directly under the module
// root). A planning-ready script's code claims cite real repo paths (e.g.
// "cmd/zing/main.go:60"), and response.CheckCodeClaims resolves that
// evidence against whatever fs.FS ValidateContext.FS names, so a real
// ValidateContext for these fixtures needs the real repo tree, not a nil
// or empty one (design section 7.3, section 11 "fixtures validate").
func repoRootFS(t *testing.T) fs.FS {
	t.Helper()
	return os.DirFS("..")
}

// jobForScript derives the running job a script's ValidateContext should
// carry from its path under fixtures/scripts, matching how the dispatcher
// actually runs each fake job (design section 6.10, 7.3): a script under
// planning/ is job.Planning; a script under build/ is job.Build. This
// mirrors checkHeader's own "document says X, run is Y" check, so a script
// whose <zing job="..."> disagrees with its own directory fails loudly
// here instead of validating under the wrong job.
func jobForScript(t *testing.T, name string) response.Job {
	t.Helper()
	switch {
	case strings.HasPrefix(name, "planning/"):
		return response.JobPlanning
	case strings.HasPrefix(name, "build/"):
		return response.JobBuild
	default:
		t.Fatalf("script %s is under neither planning/ nor build/; add a case to jobForScript", name)
		return ""
	}
}

// TestScripts_AllParseAndValidate proves every fake-runtime script under
// fixtures/scripts is a document Parse and Validate accept, offline, with
// no store or runtime involved (design section 11, "fixtures validate").
// The job handlers trust these fixtures at runtime instead of re-validating
// them (design section 6.6), so this test is what makes that trust honest.
func TestScripts_AllParseAndValidate(t *testing.T) {
	t.Parallel()

	scriptsFS, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("fs.Sub(scripts): %v", err)
	}

	var files []string
	err = fs.WalkDir(scriptsFS, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && path.Ext(p) == ".xml" {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(scripts): %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no scripts found under fixtures/scripts")
	}

	repoFS := repoRootFS(t)

	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data, readErr := fs.ReadFile(scriptsFS, name)
			if readErr != nil {
				t.Fatalf("ReadFile(%s): %v", name, readErr)
			}
			doc, parseErr := response.Parse(data)
			if parseErr != nil {
				t.Fatalf("Parse(%s): %v", name, parseErr)
			}
			// The real context the pipeline runs each script under (design
			// section 6.10, 6.6, 7.3): the job the fake was invoked for,
			// KindFeature (checkResponseExamples in cmd/zing/selftest.go
			// uses the same Kind for its own offline Validate pass), and
			// the real repo tree so a ready response's code claims resolve
			// against real paths instead of skipping that check outright.
			ctx := response.ValidateContext{Job: jobForScript(t, name), Kind: response.KindFeature, FS: repoFS}
			if errs := response.Validate(doc, ctx); len(errs) != 0 {
				t.Fatalf("Validate(%s) = %v, want no errors", name, errs)
			}
		})
	}
}

// TestScripts_InvalidDocumentReturnsErrors proves Validate actually rejects
// a broken document, so TestScripts_AllParseAndValidate passing is not
// vacuous (an inline doc, deliberately missing every required plan field
// beyond the header).
func TestScripts_InvalidDocumentReturnsErrors(t *testing.T) {
	t.Parallel()

	data := []byte(`<zing job="planning" outcome="ready"></zing>`)
	doc, err := response.Parse(data)
	if err != nil {
		t.Fatalf("Parse(invalid doc): %v", err)
	}

	errs := response.Validate(doc, response.ValidateContext{})
	if len(errs) == 0 {
		t.Fatal("Validate(invalid doc) = no errors, want at least one")
	}
}
