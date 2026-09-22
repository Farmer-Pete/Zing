package response

import (
	"regexp"
	"slices"
	"strings"
)

// proseElem is one prose text field of a Plan, at its element path (design
// section 6.4's epath grammar, fixed prefix "plan"). Only fields meant as
// human prose are collected here; a field that holds literal source code
// (Change.Before/After/SimpleCall, Migration.Schema) or a short technical
// token (an attribute, a path, a command) is not prose and carries none of
// the section 6.6 word-list rules.
type proseElem struct {
	path string
	text string
	// task marks a Task.Text element, the only prose kind the
	// performance-without-measurement rule reads (design section 6.6:
	// "a task prose element (not a change)").
	task bool
}

// proseElements walks p's prose fields in declaration order, pairing each
// with the element path CheckPlan's errors report it at.
func proseElements(p Plan) []proseElem {
	var out []proseElem
	add := func(path, text string) { out = append(out, proseElem{path: path, text: text}) }
	addAll := func(base string, items []string) {
		for i, s := range items {
			add(indexedName(base, i), s)
		}
	}

	ov := p.Overview
	add("plan/overview/objective", ov.Objective)
	add("plan/overview/context", ov.Context)
	add("plan/overview/problem", ov.Problem.Text)
	if ov.Problem.Loop != nil {
		add("plan/overview/problem/loop", ov.Problem.Loop.Text)
	}
	add("plan/overview/problem/repro", ov.Problem.Repro)
	for i, h := range ov.Problem.Hypotheses {
		base := "plan/overview/problem/hypotheses/" + indexedName("hypothesis", i)
		add(base+"/cause", h.Cause)
		add(base+"/prediction", h.Prediction)
	}
	addAll("plan/overview/goals/goal", ov.Goals)
	addAll("plan/overview/nongoals/nongoal", ov.NonGoals)

	dsg := p.Design
	add("plan/design/demo", dsg.Demo.Text)
	add("plan/design/shape", dsg.Shape)
	for i := range dsg.Changes {
		base := "plan/design/changes/" + indexedName("change", i)
		add(base+"/callers", dsg.Changes[i].Callers)
		add(base+"/callees", dsg.Changes[i].Callees)
	}
	for i, m := range dsg.Migrations.Items {
		base := "plan/design/migrations/" + indexedName("migration", i)
		add(base+"/backfill", m.Backfill)
		add(base+"/locks", m.Locks)
		add(base+"/compat", m.Compat)
		add(base+"/rollback", m.Rollback)
	}

	del := p.Delivery
	for i, f := range del.Files {
		add("plan/delivery/files/"+indexedName("file", i), f.Reason)
	}
	for i, f := range del.Deletions.Items {
		add("plan/delivery/deletions/"+indexedName("fence", i), f.ExistedBecause)
	}
	for i, tc := range del.Tests {
		add("plan/delivery/tests/"+indexedName("test", i), tc.Asserts)
	}
	for i, tk := range del.Tasks {
		out = append(out, proseElem{path: "plan/delivery/tasks/" + indexedName("task", i), text: tk.Text, task: true})
	}

	rv := p.Review
	add("plan/review/trust_root", rv.TrustRoot)
	addAll("plan/review/alternatives/alternative", rv.Alternatives)
	addAll("plan/review/risks/risk", rv.Risks)

	return out
}

// CheckPlan applies every design section 6.6 plan-checker rule to p: the
// placeholder, vague-qualifier, performance-without-measurement, and
// scenario-leak rules over its prose, plus (when bug) the bug-plan-shape
// rules. Every path is prefixed "plan". lists.Placeholders, lists.Vague,
// and lists.Units are the sole source of their respective word lists, so
// tuning checklists.toml actually changes what CheckPlan flags.
// problemPresent reports whether the document's <problem> element was
// actually present, gating the bug-shape checks that read it (see
// checkBugShape); it is meaningless, and ignored, when bug is false.
func CheckPlan(p Plan, scenarios []Scenario, bug bool, lists Checklists, problemPresent bool) []*PathError {
	var errs []*PathError

	elems := proseElements(p)
	for _, el := range elems {
		errs = append(errs, checkPlaceholders(el, lists.Placeholders)...)
		errs = append(errs, checkVague(el, lists.Vague)...)
		if el.task {
			errs = append(errs, checkPerformance(el, lists.Units)...)
		}
	}
	errs = append(errs, checkScenarioLeaks(elems, scenarios)...)

	if bug {
		errs = append(errs, checkBugShape(p, problemPresent)...)
	}

	return errs
}

// checkBugShape enforces the bug-plan-shape rules (design section 6.6),
// only called when the run's kind is bug: kind is never inferred from the
// plan's own fields (design section 10). It guards the first-test index
// with len(Tests) > 0 so a plan that also fails Layer 1's minItems=1 on
// tests cannot index past an empty slice.
//
// problemPresent reports whether the document actually carried a <problem>
// element: when it did not, p.Overview.Problem is a zero value Layer 1
// already reported missing at plan/overview/problem, and reading Loop,
// Repro, and Hypotheses off that zero value would only add a second,
// redundant set of errors at paths nested under the one already reported
// missing. The first-test-kind check does not read Problem at all, so it
// is unaffected by problemPresent.
func checkBugShape(p Plan, problemPresent bool) []*PathError {
	var errs []*PathError

	if problemPresent {
		if p.Overview.Problem.Loop == nil {
			errs = append(errs, &PathError{Path: "plan/overview/problem/loop", Msg: "bug plan needs a loop"})
		}
		if p.Overview.Problem.Repro == "" {
			errs = append(errs, &PathError{Path: "plan/overview/problem/repro", Msg: "bug plan needs a repro"})
		}
		if n := len(p.Overview.Problem.Hypotheses); n < 3 || n > 5 {
			errs = append(errs, &PathError{Path: "plan/overview/problem/hypotheses", Msg: "bug plan needs three to five hypotheses"})
		}
	}
	if len(p.Delivery.Tests) > 0 && p.Delivery.Tests[0].Kind != TestKindRegression {
		errs = append(errs, &PathError{Path: "plan/delivery/tests/test[0]/kind", Msg: "first test must be kind regression"})
	}

	return errs
}

// checkScenarioLeaks flags a plan prose element that restates a
// scenario's Then verbatim, once both sides are normalized the same way:
// trimmed, with every run of whitespace (including a line break)
// collapsed to one space (design section 6.6). An empty Then is skipped;
// Layer 1's minLength constraint on Then already reports it.
func checkScenarioLeaks(elems []proseElem, scenarios []Scenario) []*PathError {
	var errs []*PathError
	for _, el := range elems {
		normEl := normalizeWhitespace(el.text)
		for _, sc := range scenarios {
			then := normalizeWhitespace(sc.Then)
			if then == "" {
				continue
			}
			if strings.Contains(normEl, then) {
				errs = append(errs, &PathError{
					Path: el.path,
					Msg:  "repeats scenario " + sc.ID + " then-text; the plan must not restate acceptance",
				})
			}
		}
	}
	return errs
}

var whitespaceRun = regexp.MustCompile(`\s+`)

func normalizeWhitespace(s string) string {
	return whitespaceRun.ReplaceAllString(strings.TrimSpace(s), " ")
}

// performanceWordPattern matches, word-boundary and case-insensitive, any
// of the performance-claiming words design section 6.6 names.
var performanceWordPattern = regexp.MustCompile(`(?i)\b(?:optimize|optimise|optimization|optimisation|performance)\b`)

// checkPerformance flags a task's mention of optimization or performance
// that carries no measurement: a number joined to an approved unit from
// units, or a comparison against one (design section 6.6). A bare digit
// alone is not a measurement, so the rule is "no unit token found", not
// "no digit found".
func checkPerformance(el proseElem, units []string) []*PathError {
	if !performanceWordPattern.MatchString(el.text) {
		return nil
	}
	if hasMeasurement(el.text, units) {
		return nil
	}
	return []*PathError{{Path: el.path, Msg: "mentions performance without a measurement"}}
}

// hasMeasurement reports whether text contains a number immediately (with
// at most one space) followed by one of units, as a whole unit token. The
// unit alternation is sorted longest-first so a short unit (s) cannot
// pre-empt a longer one that starts the same way (ms).
//
// The trailing boundary cannot be a plain \b: a unit such as "%" ends on a
// non-word byte, and \b never matches between two non-word bytes (the
// symbol and, say, a following space or end of string), so "under 50%"
// would never match. Go's regexp (RE2) also has no lookahead to express
// "not followed by a letter or digit" directly. Instead, require the unit
// be followed by either the end of text or a non-word rune: that consumes
// one trailing rune when present, which MatchString does not mind, and it
// is unit-aware in exactly the way a fixed \b is not.
func hasMeasurement(text string, units []string) bool {
	sorted := slices.Clone(units)
	slices.SortFunc(sorted, func(a, b string) int { return len(b) - len(a) })
	alt := make([]string, 0, len(sorted))
	for _, u := range sorted {
		alt = append(alt, regexp.QuoteMeta(u))
	}
	pattern := `\d+(\.\d+)? ?(?:` + strings.Join(alt, "|") + `)(?:$|[^\p{L}\p{N}_])`
	return regexp.MustCompile(pattern).MatchString(text)
}

func checkPlaceholders(el proseElem, tokens []string) []*PathError {
	var errs []*PathError
	for _, tok := range tokens {
		if strings.Contains(el.text, tok) {
			errs = append(errs, &PathError{Path: el.path, Msg: `placeholder "` + tok + `" not allowed`})
		}
	}
	return errs
}

// checkVague flags a word-boundary, case-insensitive match of any of
// vague (already lowercased by LoadChecklists) in el's text outside a
// fenced code block (design section 6.6).
func checkVague(el proseElem, vague []string) []*PathError {
	var errs []*PathError
	outside := stripFencedCode(el.text)
	for _, word := range vague {
		if wordBoundaryMatch(outside, word) {
			errs = append(errs, &PathError{Path: el.path, Msg: `vague word "` + word + `"; give a concrete threshold`})
		}
	}
	return errs
}

// stripFencedCode returns text with every ```-fenced span replaced by a
// space, so a later word-boundary search never matches inside one. A
// fence is delimited by paired ``` markers; an odd, unpaired final marker
// is treated as opening a fence that runs to the end of the text.
func stripFencedCode(text string) string {
	parts := strings.Split(text, "```")
	var b strings.Builder
	for i, part := range parts {
		if i%2 == 0 {
			b.WriteString(part)
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// wordBoundaryMatch reports whether word appears in text as a whole word,
// case-insensitively. Plan checking runs once per response, so this
// compiles a fresh regexp each call rather than caching one: performance
// is a non-issue here (design section 0), and a shared cache would need
// its own synchronization for no real benefit.
func wordBoundaryMatch(text, word string) bool {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(word) + `\b`)
	return re.MatchString(text)
}
