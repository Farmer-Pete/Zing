// Package config loads the user/install config at ~/.zing/zing.toml.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	User     string    `toml:"user"`
	Console  Console   `toml:"console"`
	Models   Models    `toml:"models"`
	Dispatch Dispatch  `toml:"dispatch"`
	Budget   Budget    `toml:"budget"`
	Review   Review    `toml:"review"`
	Merge    Merge     `toml:"merge"`
	Projects []Project `toml:"projects"`
}

type Console struct {
	Bind      []string `toml:"bind"`
	Port      int      `toml:"port"`
	PushToken string   `toml:"push_token"`
	// AllowedHosts extends the mutation middleware's Host allowlist (design
	// section 6.14) with hostnames the middleware cannot derive on its own,
	// such as a tailnet DNS name: bare hostnames, no port. Empty by default.
	AllowedHosts []string `toml:"allowed_hosts"`
}

type Models struct {
	Sonnet string `toml:"sonnet"`
	Opus   string `toml:"opus"`
	Fable  string `toml:"fable"`
	Codex  string `toml:"codex"`
}

type Dispatch struct {
	IntervalSeconds int `toml:"interval_seconds"`
	MaxParallel     int `toml:"max_parallel"`
}

type Budget struct {
	AgentMinutesPerTicket int `toml:"agent_minutes_per_ticket"`
	UsageHoldPercent      int `toml:"usage_hold_percent"`
}

type Review struct {
	Floor string `toml:"floor"`
}

type Merge struct {
	Auto            bool     `toml:"auto"`
	Method          string   `toml:"method"`
	ManualPaths     []string `toml:"manual_paths"`
	DependencyFiles []string `toml:"dependency_files"`
}

type Project struct {
	Name     string   `toml:"name"`
	Repo     string   `toml:"repo"`
	Path     string   `toml:"path"`
	Tracker  string   `toml:"tracker"`
	Self     bool     `toml:"self"`
	Intake   Intake   `toml:"intake"`
	Commands Commands `toml:"commands"`
}

type Intake struct {
	AssignedTo string `toml:"assigned_to"`
}

type Commands struct {
	Test string `toml:"test"`
	Lint string `toml:"lint"`
}

// DefaultPath returns the default config path, ~/.zing/zing.toml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home dir: %w", err)
	}
	return filepath.Join(home, ".zing", "zing.toml"), nil
}

var (
	validReviewFloors = []string{"blocker", "major", "minor", "nit"}
	validMergeMethods = []string{"squash", "merge", "rebase"}
)

// minPushTokenLen is the shortest console.push_token Load accepts when
// zing.toml sets one explicitly (design section 6.13's bearer token gates
// every POST /subscribe and /push route). It is not applied to the
// auto-generated token cmd/zing/serve.go creates and persists to
// settings.push_token when zing.toml sets none; that value is generated at
// a fixed, already-adequate length, so this floor only catches a
// hand-written value weak enough to downgrade push auth toward a guessable
// bearer token.
const minPushTokenLen = 16

// Load reads and validates the zing.toml at path, in this exact order so the
// first reported error is deterministic: decode, unknown-key check,
// missing-required check, value checks, then defaults. Console.PushToken is
// left empty when zing.toml omits it (design section 6.13): cmd/zing/serve.go
// is the one place that resolves the effective bearer token, since only it
// can tell an explicit zing.toml value apart from a value that needs to be
// generated once and persisted to settings.push_token for stability across
// restarts -- a distinction a value regenerated fresh on every Load call
// could not preserve.
func Load(path string) (*Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("zing.toml: %w", err)
	}

	if err := checkUnknownKeys(md); err != nil {
		return nil, err
	}
	if err := checkRequiredKeys(cfg); err != nil {
		return nil, err
	}
	if err := checkValues(md, cfg); err != nil {
		return nil, err
	}

	applyDefaults(md, &cfg)

	return &cfg, nil
}

func checkUnknownKeys(md toml.MetaData) error {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	keys := make([]string, len(undecoded))
	for i, k := range undecoded {
		keys[i] = k.String()
	}
	sort.Strings(keys)
	return fmt.Errorf("zing.toml: unknown key %s", keys[0])
}

func checkRequiredKeys(cfg Config) error {
	if cfg.User == "" {
		return errors.New("zing.toml: missing required key user")
	}
	if len(cfg.Projects) == 0 {
		return errors.New("zing.toml: missing required key projects")
	}

	for i, p := range cfg.Projects {
		switch {
		case p.Name == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].name", i)
		case p.Repo == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].repo", i)
		case p.Path == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].path", i)
		case p.Tracker == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].tracker", i)
		case p.Commands.Test == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].commands.test", i)
		case p.Commands.Lint == "":
			return fmt.Errorf("zing.toml: missing required key projects[%d].commands.lint", i)
		}
	}
	return nil
}

// checkValues runs the fixed-order value checks. A field with a default
// (floor, method, port, usage_hold_percent) is only checked when the key was
// explicitly present, so an absent key defers to applyDefaults and an
// explicit out-of-range value (including an explicit zero) is rejected here.
func checkValues(md toml.MetaData, cfg Config) error {
	if md.IsDefined("review", "floor") && !slices.Contains(validReviewFloors, cfg.Review.Floor) {
		return fmt.Errorf("zing.toml: review.floor: must be one of %s", strings.Join(validReviewFloors, ", "))
	}
	if md.IsDefined("merge", "method") && !slices.Contains(validMergeMethods, cfg.Merge.Method) {
		return fmt.Errorf("zing.toml: merge.method: must be one of %s", strings.Join(validMergeMethods, ", "))
	}
	for i, p := range cfg.Projects {
		if p.Tracker != "github" {
			return fmt.Errorf("zing.toml: projects[%d].tracker: must be github", i)
		}
	}
	if md.IsDefined("console", "port") && (cfg.Console.Port < 1 || cfg.Console.Port > 65535) {
		return errors.New("zing.toml: console.port: must be 1 to 65535")
	}
	if err := checkBindAddresses(cfg.Console.Bind); err != nil {
		return err
	}
	if err := checkAllowedHosts(cfg.Console.AllowedHosts); err != nil {
		return err
	}
	if md.IsDefined("budget", "usage_hold_percent") &&
		(cfg.Budget.UsageHoldPercent < 0 || cfg.Budget.UsageHoldPercent > 100) {
		return errors.New("zing.toml: budget.usage_hold_percent: must be 0 to 100")
	}
	if md.IsDefined("console", "push_token") && len(cfg.Console.PushToken) < minPushTokenLen {
		return fmt.Errorf("zing.toml: console.push_token: must be at least %d characters", minPushTokenLen)
	}
	return nil
}

// checkBindAddresses rejects a wildcard console.bind entry (design section
// 6.14: "netip.Addr.IsUnspecified, that is 0.0.0.0 or ::"), because a
// wildcard listener has no single browser Host authority and the no-login
// console must bind concrete addresses only. A "tailscale" token, or any
// other entry that does not parse as an IP at all, is left for cmd/zing's
// resolver to handle and is not an error here.
func checkBindAddresses(bind []string) error {
	for _, b := range bind {
		addr, err := netip.ParseAddr(b)
		if err != nil {
			continue
		}
		if addr.IsUnspecified() {
			return errors.New("zing.toml: console.bind: wildcard address not allowed")
		}
	}
	return nil
}

// checkAllowedHosts rejects a console.allowed_hosts entry that carries a
// port (design section 6.14: "allowed_hosts entries are hostnames without a
// port, validated at config load").
func checkAllowedHosts(hosts []string) error {
	for i, h := range hosts {
		if _, _, err := net.SplitHostPort(h); err == nil {
			return fmt.Errorf("zing.toml: console.allowed_hosts[%d]: must not include a port", i)
		}
	}
	return nil
}

func applyDefaults(md toml.MetaData, cfg *Config) {
	if !md.IsDefined("console", "bind") {
		cfg.Console.Bind = []string{"127.0.0.1", "tailscale"}
	}
	if !md.IsDefined("console", "port") {
		cfg.Console.Port = 7420
	}
	if !md.IsDefined("models", "sonnet") {
		cfg.Models.Sonnet = "claude-sonnet-5"
	}
	if !md.IsDefined("models", "opus") {
		cfg.Models.Opus = "claude-opus-4-8"
	}
	if !md.IsDefined("models", "fable") {
		cfg.Models.Fable = "claude-fable-5-1"
	}
	if !md.IsDefined("models", "codex") {
		cfg.Models.Codex = "gpt-5.5"
	}
	if !md.IsDefined("dispatch", "interval_seconds") {
		cfg.Dispatch.IntervalSeconds = 30
	}
	if !md.IsDefined("dispatch", "max_parallel") {
		cfg.Dispatch.MaxParallel = 2
	}
	if !md.IsDefined("budget", "agent_minutes_per_ticket") {
		cfg.Budget.AgentMinutesPerTicket = 240
	}
	if !md.IsDefined("budget", "usage_hold_percent") {
		cfg.Budget.UsageHoldPercent = 80
	}
	if !md.IsDefined("review", "floor") {
		cfg.Review.Floor = "minor"
	}
	if !md.IsDefined("merge", "method") {
		cfg.Merge.Method = "squash"
	}
	if !md.IsDefined("merge", "manual_paths") {
		cfg.Merge.ManualPaths = []string{"deploy/**", "**/migrations/**", "Dockerfile", ".github/workflows/**"}
	}
	if !md.IsDefined("merge", "dependency_files") {
		cfg.Merge.DependencyFiles = []string{"go.mod", "go.sum", "package.json", "pyproject.toml"}
	}

	for i := range cfg.Projects {
		if cfg.Projects[i].Intake.AssignedTo == "" {
			cfg.Projects[i].Intake.AssignedTo = cfg.User
		}
	}
}
