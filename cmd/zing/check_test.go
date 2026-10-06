package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCheckRunsThroughSh proves "zing check SID" runs a sealed check
// through /bin/sh regardless of the caller's own login shell (#86): with
// SHELL set to /bin/zsh, a negated grep with an unquoted include glob
// behaves the same as it does under bash, so the check's real failure (a
// matching file exists) comes through as exit 1, not a false pass from
// zsh aborting on the unmatched glob before grep ever runs. Not parallel:
// it calls t.Setenv on the real process SHELL.
func TestCheckRunsThroughSh(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	const runID = int64(21)
	content := `<scenario id="s1" kind="negative" check="! grep -rn foo . --include=*.go"><given>g</given><when>w</when><then>t</then></scenario>` + "\n"
	path := writeScenariosFixture(t, t.TempDir(), runID, content)

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "a.go"), []byte("foo\n"), 0o600); err != nil {
		t.Fatalf("write a.go: %v", err)
	}

	var out, errOut bytes.Buffer
	code := check([]string{"s1"}, scenariosGetenv(map[string]string{
		scenariosTokenEnv: strconv.FormatInt(runID, 10),
		scenariosFileEnv:  path,
	}), workDir, nil, &out, &errOut)

	if code != 1 {
		t.Errorf("code = %d, want 1 (stderr: %q)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "a.go") {
		t.Errorf("stdout = %q, want it to contain %q", out.String(), "a.go")
	}
}

// TestCheckPassesCallerEnvironment proves check runs the scenario's check
// command with env as its own whole environment, not nil (review finding
// r1f4): the plan's own goal is that the caller's environment, sandbox
// variables included, survives into the check (RunShell's own doc,
// commands.go), and a check that only ever ran with env nil would still
// pass every other test in this file.
func TestCheckPassesCallerEnvironment(t *testing.T) {
	const runID = int64(22)
	content := `<scenario id="s1" kind="behavior" check="test &quot;$ZING_CHECK_PROBE&quot; = yes"><given>g</given><when>w</when><then>t</then></scenario>` + "\n"
	path := writeScenariosFixture(t, t.TempDir(), runID, content)
	getenv := scenariosGetenv(map[string]string{
		scenariosTokenEnv: strconv.FormatInt(runID, 10),
		scenariosFileEnv:  path,
	})

	var out, errOut bytes.Buffer
	code := check([]string{"s1"}, getenv, t.TempDir(), []string{"ZING_CHECK_PROBE=yes"}, &out, &errOut)
	if code != 0 {
		t.Errorf("code = %d, want 0 with ZING_CHECK_PROBE=yes in env (stderr: %q)", code, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	code = check([]string{"s1"}, getenv, t.TempDir(), nil, &out, &errOut)
	if code != 1 {
		t.Errorf("code = %d, want 1 with no env (stderr: %q)", code, errOut.String())
	}
}

// TestCheckKilledBySignal proves a check that dies by signal (rather than
// exiting on its own) exits 1 with a message naming the signal kill, not
// the raw exec.ExitError.ExitCode() of -1 (review finding r1f2): returned
// unchanged, -1 becomes the process's own exit status 255, a number the
// judge cannot tell apart from a real check exit of 255.
func TestCheckKilledBySignal(t *testing.T) {
	const runID = int64(23)
	content := `<scenario id="s1" kind="behavior" check="kill -9 $$"><given>g</given><when>w</when><then>t</then></scenario>` + "\n"
	path := writeScenariosFixture(t, t.TempDir(), runID, content)

	var out, errOut bytes.Buffer
	code := check([]string{"s1"}, scenariosGetenv(map[string]string{
		scenariosTokenEnv: strconv.FormatInt(runID, 10),
		scenariosFileEnv:  path,
	}), t.TempDir(), nil, &out, &errOut)

	if code != 1 {
		t.Errorf("code = %d, want 1 (stderr: %q)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "killed by signal") {
		t.Errorf("stderr = %q, want it to mention a signal kill", errOut.String())
	}
}

// TestCheckRefusesOutsideJudgeRun proves every refusal path exits 2 with a
// one-line stderr message and runs nothing: a canary check that would
// touch a file, reachable only past every one of these checks, must leave
// no file behind in any case.
func TestCheckRefusesOutsideJudgeRun(t *testing.T) {
	t.Parallel()
	const runID = int64(31)
	canary := filepath.Join(t.TempDir(), "canary")
	content := `<scenario id="s1" kind="behavior" check="touch ` + canary + `"><given>g</given><when>w</when><then>t</then></scenario>` + "\n" +
		`<scenario id="s2" kind="behavior"><given>g2</given><when>w2</when><then>t2</then></scenario>` + "\n"
	path := writeScenariosFixture(t, t.TempDir(), runID, content)
	token := strconv.FormatInt(runID, 10)

	cases := map[string]struct {
		args []string
		env  map[string]string
	}{
		"missing token": {
			args: []string{"s1"},
			env:  map[string]string{scenariosFileEnv: path},
		},
		"file for another run id": {
			args: []string{"s1"},
			env:  map[string]string{scenariosTokenEnv: "32", scenariosFileEnv: path},
		},
		"no args": {
			args: nil,
			env:  map[string]string{scenariosTokenEnv: token, scenariosFileEnv: path},
		},
		"unknown id": {
			args: []string{"s9"},
			env:  map[string]string{scenariosTokenEnv: token, scenariosFileEnv: path},
		},
		"no check": {
			args: []string{"s2"},
			env:  map[string]string{scenariosTokenEnv: token, scenariosFileEnv: path},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := check(tc.args, scenariosGetenv(tc.env), t.TempDir(), nil, &out, &errOut)

			if code != 2 {
				t.Errorf("code = %d, want 2", code)
			}
			if n := strings.Count(errOut.String(), "\n"); n != 1 {
				t.Errorf("stderr = %q, want exactly one line", errOut.String())
			}
			if out.String() != "" {
				t.Errorf("stdout = %q, want empty", out.String())
			}
			if _, statErr := os.Stat(canary); statErr == nil {
				t.Error("canary file exists: check ran the scenario's command despite being refused")
			}
		})
	}
}
