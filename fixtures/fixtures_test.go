package fixtures_test

import (
	"io/fs"
	"path"
	"testing"

	"zing/fixtures"
	"zing/internal/response"
)

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
			if errs := response.Validate(doc, response.ValidateContext{}); len(errs) != 0 {
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
