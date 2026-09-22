package response

import (
	"reflect"
	"testing"
)

func findChild(n *node, name string) *node {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func childNames(n *node) []string {
	names := make([]string, 0, len(n.Children))
	for _, c := range n.Children {
		names = append(names, c.Name)
	}
	return names
}

func TestShapeOf_HeadFlattenedIntoAttrs(t *testing.T) {
	t.Parallel()

	n := shapeOf(reflect.TypeFor[ClassifyResponse]())

	job := findChild(n, "job")
	if job == nil {
		t.Fatal("job attribute not found")
	}
	if job.Kind != kindAttr {
		t.Errorf("job.Kind = %v, want kindAttr", job.Kind)
	}

	outcome := findChild(n, "outcome")
	if outcome == nil {
		t.Fatal("outcome attribute not found")
	}
	if outcome.Kind != kindAttr {
		t.Errorf("outcome.Kind = %v, want kindAttr", outcome.Kind)
	}

	for _, name := range childNames(n) {
		if name == "XMLName" || name == "zing" {
			t.Errorf("XMLName leaked into the shape as a child named %q", name)
		}
	}
}

func TestShapeOf_AttrsBeforeElements(t *testing.T) {
	t.Parallel()

	n := shapeOf(reflect.TypeFor[ClassifyResponse]())
	names := childNames(n)
	want := []string{"job", "outcome", "reason"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("children = %v, want %v", names, want)
	}
}

func TestShapeOf_RequiredScalarElement(t *testing.T) {
	t.Parallel()

	n := shapeOf(reflect.TypeFor[ClassifyResponse]())
	reason := findChild(n, "reason")
	if reason == nil {
		t.Fatal("reason child not found")
	}
	if reason.Kind != kindElement {
		t.Errorf("reason.Kind = %v, want kindElement", reason.Kind)
	}
	if reason.Slice {
		t.Error("reason should not be a slice")
	}
	if !reason.Required {
		t.Error("reason should be required (its xml tag has no omitempty)")
	}
	if !reason.HasMinLength || reason.MinLength != 1 {
		t.Errorf("reason minLength = (%d, %v), want (1, true)", reason.MinLength, reason.HasMinLength)
	}
}

func TestShapeOf_RequiredSliceGovernedByMinItems(t *testing.T) {
	t.Parallel()

	n := shapeOf(reflect.TypeFor[ReadyResponse]())
	claims := findChild(n, "claims")
	if claims == nil {
		t.Fatal("claims wrapper not found")
	}
	if claims.Kind != kindWrapper {
		t.Errorf("claims.Kind = %v, want kindWrapper", claims.Kind)
	}
	if len(claims.Children) != 1 {
		t.Fatalf("claims.Children has %d entries, want 1", len(claims.Children))
	}
	claim := claims.Children[0]
	if claim.Name != "claim" {
		t.Errorf("wrapper child name = %q, want claim", claim.Name)
	}
	if !claim.Slice {
		t.Error("claim should be a slice")
	}
	if !claim.Required {
		t.Error("claim should be required: its field has minItems=1")
	}
	if !claim.HasMinItems || claim.MinItems != 1 {
		t.Errorf("claim minItems = (%d, %v), want (1, true)", claim.MinItems, claim.HasMinItems)
	}
}

func TestShapeOf_OptionalSliceHasNoMinItems(t *testing.T) {
	t.Parallel()

	// Question.Options has jsonschema:"maxItems=4" but no minItems, so it
	// must be optional (design section 6.4: absent minItems means zero).
	qType := shapeOf(reflect.TypeFor[Question]())
	opt := findChild(qType, "option")
	if opt == nil {
		t.Fatal("option child not found")
	}
	if opt.Required {
		t.Error("option should be optional: Question.Options has no minItems")
	}
	if !opt.HasMaxItems || opt.MaxItems != 4 {
		t.Errorf("option maxItems = (%d, %v), want (4, true)", opt.MaxItems, opt.HasMaxItems)
	}
	if opt.HasMinItems {
		t.Error("option should have no minItems constraint")
	}
}

func TestShapeOf_DropsXMLTagDash(t *testing.T) {
	t.Parallel()

	n := shapeOf(reflect.TypeFor[Finding]())
	if findChild(n, "decision") != nil {
		t.Error(`Finding.Decision is tagged xml:"-" and must not appear in the shape`)
	}
}

func TestShapeOf_OptionalPointerElement(t *testing.T) {
	t.Parallel()

	n := shapeOf(reflect.TypeFor[Problem]())
	loop := findChild(n, "loop")
	if loop == nil {
		t.Fatal("loop child not found")
	}
	if loop.Required {
		t.Error("loop should be optional: Problem.Loop has omitempty")
	}
	if len(loop.Children) == 0 {
		t.Error("loop should have children reflected from *Loop's struct fields")
	}
}

func TestShapeOf_Chardata(t *testing.T) {
	t.Parallel()

	n := shapeOf(reflect.TypeFor[Fence]())
	var chardata *node
	for _, c := range n.Children {
		if c.Kind == kindChardata {
			chardata = c
		}
	}
	if chardata == nil {
		t.Fatal("Fence should have a chardata child for ExistedBecause")
	}
	if chardata.Name != "" {
		t.Errorf("chardata child Name = %q, want empty", chardata.Name)
	}
	if !chardata.HasPattern || chardata.Pattern != "existed because" {
		t.Errorf("chardata pattern = (%q, %v), want (\"existed because\", true)", chardata.Pattern, chardata.HasPattern)
	}
}

func TestShapeOf_EnumDetection(t *testing.T) {
	t.Parallel()

	n := shapeOf(reflect.TypeFor[Claim]())
	kind := findChild(n, "kind")
	if kind == nil {
		t.Fatal("kind attribute not found")
	}
	if !kind.Enum {
		t.Error("kind should be detected as an enum: ClaimKind has a Values() method")
	}
}

func TestShapeOf_Cached(t *testing.T) {
	t.Parallel()

	a := shapeOf(reflect.TypeFor[ClassifyResponse]())
	b := shapeOf(reflect.TypeFor[ClassifyResponse]())
	if a != b {
		t.Error("shapeOf should cache and return the same *node pointer for repeat calls on the same type")
	}
}

func TestShapeOf_NumericBounds(t *testing.T) {
	t.Parallel()

	n := shapeOf(reflect.TypeFor[Hypothesis]())
	rank := findChild(n, "rank")
	if rank == nil {
		t.Fatal("rank attribute not found")
	}
	if !rank.HasMinimum || rank.Minimum != 1 {
		t.Errorf("rank minimum = (%v, %v), want (1, true)", rank.Minimum, rank.HasMinimum)
	}
	if !rank.HasMaximum || rank.Maximum != 5 {
		t.Errorf("rank maximum = (%v, %v), want (5, true)", rank.Maximum, rank.HasMaximum)
	}
}
