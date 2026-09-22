package response

import (
	"slices"
	"strings"
)

// semantics.go holds the structural Layer 2 checks (design section 6.6):
// they read attributes and lists, not prose, unlike plancheck.go's
// word-list rules.

// checkNoneUnion enforces the none/list union that Migrations and
// Deletions both carry: either none="true" and an empty list, or no none
// attribute and a non-empty list. path is the containing element's own
// path (e.g. "plan/design/migrations"); nonePresent reports whether the
// none attribute appeared at all in the document (Layer 1's presence set),
// since an omitted attribute and one written none="false" both decode to
// the same Go zero value and are only distinguishable through presence;
// none is that decoded value; itemCount is the item list's length.
func checkNoneUnion(path string, nonePresent, none bool, itemCount int) *PathError {
	switch {
	case nonePresent && !none:
		return &PathError{Path: path, Msg: `none must be "true" when present`}
	case nonePresent && itemCount > 0:
		return &PathError{Path: path, Msg: `none="true" allows no items`}
	case !nonePresent && itemCount == 0:
		return &PathError{Path: path, Msg: `set none="true" or list one or more`}
	default:
		return nil
	}
}

// checkChildrenDAG checks a ChildrenResponse's children (design section
// 6.6): every key is unique, every depends_on names an existing key with
// no self-dependency, and the depends_on edges form no cycle.
func checkChildrenDAG(children []Child) []*PathError {
	var errs []*PathError

	seen := make(map[string]bool, len(children))
	for i, c := range children {
		if seen[c.Key] {
			errs = append(errs, &PathError{
				Path: indexedName("child", i) + "/key",
				Msg:  "duplicate key " + c.Key,
			})
		}
		seen[c.Key] = true
	}

	for i, c := range children {
		path := indexedName("child", i) + "/depends_on"
		for _, dep := range c.DependsOn {
			switch {
			case dep == c.Key:
				errs = append(errs, &PathError{Path: path, Msg: "self-dependency"})
			case !seen[dep]:
				errs = append(errs, &PathError{Path: path, Msg: "unknown key " + dep})
			}
		}
	}

	if cycle := findDependencyCycle(children); cycle != nil {
		errs = append(errs, &PathError{Path: "children", Msg: "dependency cycle " + strings.Join(cycle, " -> ")})
	}

	return errs
}

// findDependencyCycle runs a DFS over the children's depends_on edges, in
// child declaration order, and in each child's own depends_on order, so
// the first cycle found (and the path reported for it) is deterministic.
// A self-dependency edge is excluded from the graph, since checkChildrenDAG
// already reports it as its own rule; an edge to an unknown key is
// excluded too, since there is no node to walk into.
func findDependencyCycle(children []Child) []string {
	byKey := make(map[string]Child, len(children))
	for _, c := range children {
		byKey[c.Key] = c
	}

	const (
		unvisited = iota
		onStack
		done
	)
	state := make(map[string]int, len(children))
	var stack []string

	var visit func(key string) []string
	visit = func(key string) []string {
		state[key] = onStack
		stack = append(stack, key)
		for _, dep := range byKey[key].DependsOn {
			if dep == key {
				continue
			}
			next, ok := byKey[dep]
			if !ok {
				continue
			}
			switch state[dep] {
			case onStack:
				start := slices.Index(stack, dep)
				return append(slices.Clone(stack[start:]), dep)
			case unvisited:
				if cycle := visit(next.Key); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[key] = done
		return nil
	}

	for _, c := range children {
		if state[c.Key] == unvisited {
			if cycle := visit(c.Key); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// checkQuestionCardinality enforces each question's option count (design
// section 6.6): none, or two to four. Layer 1's maxItems=4 constraint
// already flags a count over four with its own message; this is the rule
// that catches exactly one, the shape that constraint alone cannot name.
func checkQuestionCardinality(questions []Question) []*PathError {
	var errs []*PathError
	for i, q := range questions {
		n := len(q.Options)
		if n == 1 || n > 4 {
			errs = append(errs, &PathError{
				Path: indexedName("question", i) + "/options",
				Msg:  "give none, or two to four",
			})
		}
	}
	return errs
}
