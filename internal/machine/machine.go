// Package machine loads and validates machine.toml, the process definition
// every later Zing package reads: the jobs and the pipeline states.
package machine

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"

	"github.com/BurntSushi/toml"

	"zing/internal/response"
)

type Machine struct {
	Version int            `toml:"version"`
	Jobs    map[string]Job `toml:"jobs"`
	States  States         `toml:"states"`
}

type Job struct {
	Model          string    `toml:"model"`
	Runtime        string    `toml:"runtime"`
	Tools          []string  `toml:"tools"`
	Prompt         PromptRef `toml:"prompt"`
	Style          []string  `toml:"style"`
	Lenses         []string  `toml:"lenses"`
	Session        string    `toml:"session"`
	Per            string    `toml:"per"`
	Worktree       string    `toml:"worktree"`
	TimeoutMinutes int       `toml:"timeout_minutes"`
	MaxResumes     int       `toml:"max_resumes"` // default 1 when the key is absent
	MaxLoops       int       `toml:"max_loops"`
	Outcomes       []string  `toml:"outcomes"` // default ["ok"] when the key is absent
}

// PromptRef is a job's prompt: either a single path, or a {feature, bug}
// pair for planning's two prompt bodies. UnmarshalTOML never hard-errors;
// any invalid shape sets bad so validation reports the one canonical error.
type PromptRef struct {
	Single, Feature, Bug string
	bad                  bool
}

func (p *PromptRef) UnmarshalTOML(data any) error {
	switch v := data.(type) {
	case string:
		p.Single = v
	case map[string]any:
		for key, val := range v {
			s, ok := val.(string)
			if !ok {
				p.bad = true
				continue
			}
			switch key {
			case "feature":
				p.Feature = s
			case "bug":
				p.Bug = s
			default:
				p.bad = true
			}
		}
	default:
		p.bad = true
	}
	return nil
}

type States struct {
	Order    []string `toml:"order"`
	Terminal []string `toml:"terminal"`
}

// toolCatalogue is every tool a job may request.
var toolCatalogue = map[string]bool{
	"read": true, "grep": true, "glob": true, "bash_readonly": true,
	"edit": true, "write": true, "bash": true,
}

var validModels = map[string]bool{"sonnet": true, "opus": true, "fable": true, "codex": true}

var validRuntimes = map[string]bool{"claude": true, "codex": true, "fake": true}

// Load reads and validates machine.toml at path within fsys.
func Load(fsys fs.FS, path string) (*Machine, error) {
	var m Machine
	md, err := toml.DecodeFS(fsys, path, &m)
	if err != nil {
		return nil, fmt.Errorf("machine.toml: %w", err)
	}

	if err := validateTopLevel(m); err != nil {
		return nil, err
	}

	names := sortedJobNames(m.Jobs)
	for _, name := range names {
		job := m.Jobs[name]
		applyJobDefaults(md, name, &job)
		m.Jobs[name] = job
	}
	for _, name := range names {
		if err := validateJob(fsys, md, name, m.Jobs[name]); err != nil {
			return nil, err
		}
	}

	return &m, nil
}

func sortedJobNames(jobs map[string]Job) []string {
	names := make([]string, 0, len(jobs))
	for name := range jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// applyJobDefaults materializes the two absent-key defaults: MaxResumes
// becomes 1, and Outcomes becomes ["ok"], each only when its key was absent
// from machine.toml (an explicit 0 or [] is left alone for validation).
func applyJobDefaults(md toml.MetaData, name string, job *Job) {
	if !md.IsDefined("jobs", name, "max_resumes") {
		job.MaxResumes = 1
	}
	if !md.IsDefined("jobs", name, "outcomes") {
		job.Outcomes = []string{"ok"}
	}
}

func validateTopLevel(m Machine) error {
	if m.Version != 1 {
		return errors.New("machine.toml: version: must be 1")
	}
	if len(m.Jobs) == 0 {
		return errors.New("machine.toml: jobs: at least one job required")
	}
	if err := validateStateList("states.order", m.States.Order); err != nil {
		return err
	}
	if err := validateStateList("states.terminal", m.States.Terminal); err != nil {
		return err
	}
	return nil
}

func validateStateList(field string, list []string) error {
	if len(list) == 0 {
		return fmt.Errorf("machine.toml: %s: must not be empty", field)
	}
	for _, name := range list {
		if !isTicketState(name) {
			return fmt.Errorf("machine.toml: %s: unknown state %s", field, name)
		}
	}
	seen := make(map[string]bool, len(list))
	for _, name := range list {
		if seen[name] {
			return fmt.Errorf("machine.toml: %s: duplicate state %s", field, name)
		}
		seen[name] = true
	}
	return nil
}

func isTicketState(name string) bool {
	return slices.Contains(response.TicketState("").Values(), name)
}

func isOutcome(name string) bool {
	return slices.Contains(response.Outcome("").Values(), name)
}

// validateJob checks one job's fields in the fixed order the plan pins, so
// the first failure is the reported error.
func validateJob(fsys fs.FS, md toml.MetaData, name string, job Job) error {
	jobErr := func(field, reason string) error {
		return fmt.Errorf("machine.toml: job %s: %s: %s", name, field, reason)
	}

	if !validModels[job.Model] {
		return jobErr("model", "must be one of sonnet, opus, fable, codex")
	}
	if !validRuntimes[job.Runtime] {
		return jobErr("runtime", "must be one of claude, codex, fake")
	}
	if job.Session != "" && job.Session != "long" {
		return jobErr("session", "must be absent or long")
	}
	if job.Per != "" && job.Per != "task" && job.Per != "lens" {
		return jobErr("per", "must be absent, task, or lens")
	}
	if job.Worktree != "" && job.Worktree != "sparse" {
		return jobErr("worktree", "must be absent or sparse")
	}
	for _, tool := range job.Tools {
		if !toolCatalogue[tool] {
			return jobErr("tools", "unknown tool "+tool)
		}
	}
	if job.MaxResumes < 0 || job.MaxResumes > 20 {
		return jobErr("max_resumes", "must be 0 to 20")
	}
	if md.IsDefined("jobs", name, "max_loops") && (job.MaxLoops < 1 || job.MaxLoops > 5) {
		return jobErr("max_loops", "must be 1 to 5")
	}
	if job.TimeoutMinutes < 1 || job.TimeoutMinutes > 240 {
		return jobErr("timeout_minutes", "must be 1 to 240")
	}
	if err := validateOutcomes(jobErr, job.Outcomes); err != nil {
		return err
	}
	if err := validatePromptShape(jobErr, job.Prompt); err != nil {
		return err
	}
	return validatePaths(fsys, jobErr, job)
}

// validateOutcomes checks the outcomes list against the reason-table
// precedence in plan section 7.3-7.5: empty, then unknown, then duplicate,
// then universal. Each check walks the whole list in order and reports its
// first offender, so the reported error depends on the check's rank, not on
// which element happens to come first in the list.
func validateOutcomes(jobErr func(field, reason string) error, outcomes []string) error {
	if len(outcomes) == 0 {
		return jobErr("outcomes", "must not be empty")
	}
	for _, v := range outcomes {
		if !isOutcome(v) {
			return jobErr("outcomes", "unknown outcome "+v)
		}
	}
	seen := make(map[string]bool, len(outcomes))
	for _, v := range outcomes {
		if seen[v] {
			return jobErr("outcomes", "duplicate "+v)
		}
		seen[v] = true
	}
	for _, v := range outcomes {
		if v == "question" || v == "error" {
			return jobErr("outcomes", "question and error are universal, not listed")
		}
	}
	return nil
}

func validatePromptShape(jobErr func(field, reason string) error, p PromptRef) error {
	single := p.Single != ""
	pair := p.Feature != "" && p.Bug != ""
	if p.bad || (!single && !pair) || (single && pair) {
		return jobErr("prompt", "must be a path or a {feature,bug} pair")
	}
	return nil
}

// validatePaths checks path existence, in the order: the set prompt path(s),
// then style in list order, then lenses in list order.
func validatePaths(fsys fs.FS, jobErr func(field, reason string) error, job Job) error {
	var promptPaths []string
	switch {
	case job.Prompt.Single != "":
		promptPaths = []string{job.Prompt.Single}
	default:
		promptPaths = []string{job.Prompt.Feature, job.Prompt.Bug}
	}
	for _, p := range promptPaths {
		if err := statPath(fsys, jobErr, "prompt", p); err != nil {
			return err
		}
	}
	for _, p := range job.Style {
		if err := statPath(fsys, jobErr, "style", p); err != nil {
			return err
		}
	}
	for _, lens := range job.Lenses {
		if err := statPath(fsys, jobErr, "lens", "prompts/lenses/"+lens+".md"); err != nil {
			return err
		}
	}
	return nil
}

func statPath(fsys fs.FS, jobErr func(field, reason string) error, field, path string) error {
	info, err := fs.Stat(fsys, path)
	if err != nil {
		return jobErr(field, "file not found: "+path)
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		return jobErr(field, "not a regular file: "+path)
	}
	return nil
}
