package store

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

//go:embed schemas
var schemaFS embed.FS

//go:embed examples
var examplesFS embed.FS

// SchemaFS returns the embedded committed schema tree, sub-rooted at
// "schemas" so paths read as "messages/state.json". Used by selftest's
// schema-drift check.
func SchemaFS() fs.FS {
	sub, err := fs.Sub(schemaFS, "schemas")
	if err != nil {
		// schemas is embedded at build time; a missing "schemas" subtree is a
		// programming error, not a runtime condition.
		panic(fmt.Sprintf("sub schemas fs: %v", err))
	}
	return sub
}

// schemaSet holds every compiled stored-type schema, keyed "table/type"
// (for example "artifacts/scenario").
type schemaSet struct {
	compiled map[string]*jsonschema.Schema
}

// loadSchemas compiles every committed schema under schemas/.
func loadSchemas() (*schemaSet, error) {
	sub := SchemaFS()

	compiler := jsonschema.NewCompiler()
	var relpaths []string
	err := fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(p) != ".json" {
			return nil
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return fmt.Errorf("read schema %s: %w", p, err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("unmarshal schema %s: %w", p, err)
		}
		if err := compiler.AddResource(p, doc); err != nil {
			return fmt.Errorf("add schema resource %s: %w", p, err)
		}
		relpaths = append(relpaths, p)
		return nil
	})
	if err != nil {
		return nil, err
	}

	compiled := make(map[string]*jsonschema.Schema, len(relpaths))
	for _, p := range relpaths {
		sch, err := compiler.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("compile schema %s: %w", p, err)
		}
		compiled[strings.TrimSuffix(p, ".json")] = sch
	}

	return &schemaSet{compiled: compiled}, nil
}

// validate checks payload against the compiled schema for table/typ.
func (s *schemaSet) validate(table, typ string, payload []byte) error {
	sch, ok := s.compiled[table+"/"+typ]
	if !ok {
		return fmt.Errorf("payload does not match schema %s: /: no schema for %s/%s", typ, table, typ)
	}

	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return fmt.Errorf("payload does not match schema %s: /: invalid JSON: %w", typ, err)
	}

	if err := sch.Validate(v); err != nil {
		if verr, ok := errors.AsType[*jsonschema.ValidationError](err); ok {
			loc, reason := firstLeaf(verr)
			return fmt.Errorf("payload does not match schema %s: %s: %s", typ, loc, reason)
		}
		return fmt.Errorf("payload does not match schema %s: /: %w", typ, err)
	}
	return nil
}

// firstLeaf walks the whole validation error tree, collects every leaf (a
// node with no causes), and returns the (path, reason) of the first leaf
// sorted by path then reason. jsonschema/v6 walks object properties by
// ranging a Go map, so Causes[0] alone is not deterministic; sorting every
// leaf is.
func firstLeaf(verr *jsonschema.ValidationError) (loc, reason string) {
	printer := message.NewPrinter(language.English)

	type entry struct{ loc, reason string }
	var leaves []entry

	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			leaves = append(leaves, entry{
				loc:    instancePointer(e.InstanceLocation),
				reason: e.ErrorKind.LocalizedString(printer),
			})
			return
		}
		for _, cause := range e.Causes {
			walk(cause)
		}
	}
	walk(verr)

	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].loc != leaves[j].loc {
			return leaves[i].loc < leaves[j].loc
		}
		return leaves[i].reason < leaves[j].reason
	})
	return leaves[0].loc, leaves[0].reason
}

// instancePointer builds the RFC 6901 JSON Pointer for an instance location,
// escaping ~ and / in each segment. An empty location is the root pointer "/".
func instancePointer(segments []string) string {
	if len(segments) == 0 {
		return "/"
	}
	escaper := strings.NewReplacer("~", "~0", "/", "~1")
	var b strings.Builder
	for _, seg := range segments {
		b.WriteByte('/')
		b.WriteString(escaper.Replace(seg))
	}
	return b.String()
}

// ValidateExamples validates every committed example under examples/ against
// its compiled schema, in sorted table/type order. It returns the first
// failure wrapped as "example <table>/<type>: <err>".
func (s *Store) ValidateExamples() error {
	sub, err := fs.Sub(examplesFS, "examples")
	if err != nil {
		return fmt.Errorf("sub examples fs: %w", err)
	}

	keys := make([]string, 0, len(s.schemas.compiled))
	for key := range s.schemas.compiled {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		table, typ, ok := strings.Cut(key, "/")
		if !ok {
			return fmt.Errorf("malformed schema key %q", key)
		}
		b, err := fs.ReadFile(sub, key+".json")
		if err != nil {
			return fmt.Errorf("example %s/%s: %w", table, typ, err)
		}
		if err := s.schemas.validate(table, typ, b); err != nil {
			return fmt.Errorf("example %s/%s: %w", table, typ, err)
		}
	}
	return nil
}
