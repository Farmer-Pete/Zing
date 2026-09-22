package response

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// nodeKind is what an XML shape node stands for: an attribute, a struct's
// own text, a child element, or a synthetic wrapper introduced by an
// xml:"a>b" tag.
type nodeKind int

const (
	kindAttr nodeKind = iota
	kindChardata
	kindElement
	kindWrapper
)

// node is one field's reflected XML shape: its wire name, its kind, whether
// it repeats, whether it is required, its jsonschema constraints, and (for
// a struct-typed element or a wrapper) its children in the order design
// section 6.4 requires: every attribute ahead of every chardata, element,
// or wrapper child.
//
// Get reads this field's value out of the reflect.Value of the struct that
// declares it (the immediate parent in the shape tree, not necessarily the
// top-level response). It is nil for kindWrapper nodes, which have no Go
// value of their own; their single child's Get reads directly from the
// wrapper's own parent.
type node struct {
	Name     string
	Kind     nodeKind
	Slice    bool
	Required bool

	HasMinLength bool
	MinLength    int
	HasMaxItems  bool
	MaxItems     int
	HasMinItems  bool
	MinItems     int
	HasMinimum   bool
	Minimum      float64
	HasMaximum   bool
	Maximum      float64
	HasPattern   bool
	Pattern      string
	Enum         bool

	// Doc is the field's doc struct tag, verbatim. Empty when the field
	// carries none. Read by the renderer (render.go); Layer 1 validation
	// does not use it.
	Doc string
	// GoType is the field's own value type: the attribute or chardata
	// field's type, or an element field's (slice-)element type, always
	// dereferenced through a pointer. Nil for a kindWrapper node, which
	// has no value of its own. Read by the renderer to print a field's
	// type name (string, int, bool, or an enum or struct name).
	GoType reflect.Type

	Children []*node
	Get      func(reflect.Value) reflect.Value
}

var (
	shapeMu    sync.RWMutex
	shapeCache = map[reflect.Type]*node{}
	headType   = reflect.TypeFor[Head]()
	valuesType = reflect.TypeFor[interface{ Values() []string }]()
)

// shapeOf returns the reflected XML shape descriptor for t, caching it so
// repeated callers (the parser, the validator, the renderer) share one
// descriptor per type.
func shapeOf(t reflect.Type) *node {
	shapeMu.RLock()
	n, ok := shapeCache[t]
	shapeMu.RUnlock()
	if ok {
		return n
	}

	shapeMu.Lock()
	defer shapeMu.Unlock()
	if cached, ok := shapeCache[t]; ok {
		return cached
	}
	n = &node{Kind: kindElement, Children: buildChildren(t)}
	shapeCache[t] = n
	return n
}

// buildChildren reflects t's exported fields into shape nodes, in
// declaration order but with every attribute ahead of every chardata,
// element, or wrapper child (design section 6.4). It drops xml:"-" fields,
// skips XMLName (decoder metadata), and flattens an embedded Head so job
// and outcome become attributes on the caller's own node.
func buildChildren(t reflect.Type) []*node {
	var attrs, rest []*node

	for i := range t.NumField() {
		f := t.Field(i)
		idx := i
		get := func(v reflect.Value) reflect.Value { return v.Field(idx) }

		if f.Anonymous && f.Type == headType {
			for _, c := range buildChildren(f.Type) {
				flattened := composeGet(get, c)
				if flattened.Kind == kindAttr {
					attrs = append(attrs, flattened)
				} else {
					rest = append(rest, flattened)
				}
			}
			continue
		}
		if f.Name == "XMLName" {
			continue
		}
		tag, ok := f.Tag.Lookup("xml")
		if !ok || tag == "-" {
			continue
		}

		n := fieldNode(f, tag)
		if n == nil {
			continue
		}
		if n.Kind == kindWrapper {
			n.Children[0].Get = get
			rest = append(rest, n)
			continue
		}
		n.Get = get
		if n.Kind == kindAttr {
			attrs = append(attrs, n)
		} else {
			rest = append(rest, n)
		}
	}

	return append(attrs, rest...)
}

// composeGet returns a copy of c whose Get (or, for a wrapper, whose single
// child's Get) reads through outer first, so a flattened embedded field's
// accessor still reaches its value from the embedding struct.
func composeGet(outer func(reflect.Value) reflect.Value, c *node) *node {
	cp := *c
	if cp.Kind == kindWrapper {
		innerCopy := *cp.Children[0]
		inner := cp.Children[0].Get
		innerCopy.Get = func(v reflect.Value) reflect.Value { return inner(outer(v)) }
		cp.Children = []*node{&innerCopy}
		return &cp
	}
	inner := c.Get
	cp.Get = func(v reflect.Value) reflect.Value { return inner(outer(v)) }
	return &cp
}

// fieldNode builds the shape node for one struct field from its raw xml
// tag. It returns a kindWrapper node (with Get unset on its single child)
// when the tag uses the xml:"a>b" wrapper syntax.
func fieldNode(f reflect.StructField, tag string) *node {
	parts := strings.Split(tag, ",")
	name := parts[0]
	opts := parts[1:]

	switch {
	case hasOpt(opts, "attr"):
		n := &node{Kind: kindAttr, Name: name, Required: !hasOpt(opts, "omitempty"), Doc: f.Tag.Get("doc"), GoType: f.Type}
		applyConstraints(n, f)
		n.Enum = hasValuesMethod(f.Type)
		return n
	case name == "" && hasOpt(opts, "chardata"):
		n := &node{Kind: kindChardata, Required: !hasOpt(opts, "omitempty"), Doc: f.Tag.Get("doc"), GoType: f.Type}
		applyConstraints(n, f)
		return n
	default:
		if outer, inner, ok := strings.Cut(name, ">"); ok {
			return &node{Kind: kindWrapper, Name: outer, Children: []*node{elementNode(inner, f)}}
		}
		return elementNode(name, f)
	}
}

// elementNode builds the shape node for a plain or repeated child element
// named name, whose Go value comes from field f (a slice field when it
// repeats, directly or under a wrapper).
func elementNode(name string, f reflect.StructField) *node {
	t := f.Type
	slice := t.Kind() == reflect.Slice
	elemType := t
	if slice {
		elemType = t.Elem()
	}
	if elemType.Kind() == reflect.Pointer {
		elemType = elemType.Elem()
	}

	n := &node{Kind: kindElement, Name: name, Slice: slice, Doc: f.Tag.Get("doc"), GoType: elemType}
	if elemType.Kind() == reflect.Struct {
		n.Children = buildChildren(elemType)
	}
	applyConstraints(n, f)

	if slice {
		n.Required = n.HasMinItems
	} else {
		opts := strings.Split(f.Tag.Get("xml"), ",")[1:]
		n.Required = !hasOpt(opts, "omitempty")
	}
	n.Enum = hasValuesMethod(t)
	return n
}

func hasOpt(opts []string, want string) bool {
	return slices.Contains(opts, want)
}

// applyConstraints reads f's jsonschema tag into n's constraint fields.
func applyConstraints(n *node, f reflect.StructField) {
	tag, ok := f.Tag.Lookup("jsonschema")
	if !ok {
		return
	}
	for part := range strings.SplitSeq(tag, ",") {
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch key {
		case "minLength":
			if v, err := strconv.Atoi(val); err == nil {
				n.MinLength, n.HasMinLength = v, true
			}
		case "maxItems":
			if v, err := strconv.Atoi(val); err == nil {
				n.MaxItems, n.HasMaxItems = v, true
			}
		case "minItems":
			if v, err := strconv.Atoi(val); err == nil {
				n.MinItems, n.HasMinItems = v, true
			}
		case "minimum":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				n.Minimum, n.HasMinimum = v, true
			}
		case "maximum":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				n.Maximum, n.HasMaximum = v, true
			}
		case "pattern":
			n.Pattern, n.HasPattern = val, true
		}
	}
}

// hasValuesMethod reports whether t (or *t) implements Values() []string,
// the marker this package's enum types share (job.go, enums.go).
func hasValuesMethod(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Implements(valuesType) || reflect.PointerTo(t).Implements(valuesType)
}
