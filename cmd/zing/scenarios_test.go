package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// scenariosTokenEnv and scenariosFileEnv name the two env vars scenarios
// reads, so every fixture's getenv stub agrees with them (goconst).
const (
	scenariosTokenEnv = "ZING_RUN_TOKEN"
	scenariosFileEnv  = "ZING_SCENARIOS_FILE"
)

// scenariosGetenv builds a getenv stub serving exactly the env vars in vals
// (ZING_RUN_TOKEN, ZING_SCENARIOS_FILE, or anything else a test wants to
// probe), "" for every other name.
func scenariosGetenv(vals map[string]string) func(string) string {
	return func(k string) string { return vals[k] }
}

// writeScenariosFixture writes content at <dir>/judge/<runID>/scenarios.xml
// (dir 0700, file 0600, internal/job/judging.go's own writeScenariosFile
// shape) and returns the file's path.
func writeScenariosFixture(t *testing.T, dir string, runID int64, content string) string {
	t.Helper()
	runDir := filepath.Join(dir, "judge", strconv.FormatInt(runID, 10))
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", runDir, err)
	}
	path := filepath.Join(runDir, "scenarios.xml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestScenariosPrintsFile proves the happy path (PKG9-PLAN.md section 7.3):
// a ZING_RUN_TOKEN naming a positive decimal run id and a ZING_SCENARIOS_FILE
// ending in "/judge/<token>/scenarios.xml" is copied to stdout byte for
// byte, with nothing on stderr.
func TestScenariosPrintsFile(t *testing.T) {
	t.Parallel()
	const runID = int64(42)
	const content = `<scenario id="s1" kind="behavior"><given>g</given><when>w</when><then>t</then></scenario>` + "\n" +
		`<scenario id="s2" kind="negative" check="go test -run &#34;A &amp;&amp; B &lt; C&#34;"><given>g2</given><when>w2</when><then>t2</then></scenario>` + "\n"
	path := writeScenariosFixture(t, t.TempDir(), runID, content)

	var out, errOut bytes.Buffer
	code := scenarios(scenariosGetenv(map[string]string{
		scenariosTokenEnv: strconv.FormatInt(runID, 10),
		scenariosFileEnv:  path,
	}), &out, &errOut)

	if code != 0 {
		t.Errorf("code = %d, want 0 (stderr: %q)", code, errOut.String())
	}
	if out.String() != content {
		t.Errorf("stdout = %q, want %q", out.String(), content)
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want empty", errOut.String())
	}
}

// TestScenariosNoRunToken covers every "no run context" trigger (PKG9-PLAN.md
// section 7.3 step 1): a missing ZING_RUN_TOKEN, a non-numeric one, zero, a
// negative one, and one with a leading zero (parseRunToken's own exact
// round-trip check) -- each exits 2 with the exact stderr text and nothing
// on stdout, regardless of ZING_SCENARIOS_FILE.
func TestScenariosNoRunToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeScenariosFixture(t, dir, 1, "<scenario></scenario>\n")

	cases := map[string]string{
		"missing token":  "",
		"non-numeric":    "abc",
		"zero":           "0",
		"negative":       "-1",
		"leading zero":   "01",
		"trailing space": "1 ", // not a clean decimal round-trip
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := scenarios(scenariosGetenv(map[string]string{
				scenariosTokenEnv: token,
				scenariosFileEnv:  path,
			}), &out, &errOut)

			if code != 2 {
				t.Errorf("code = %d, want 2", code)
			}
			if want := scenariosNoRunContext + "\n"; errOut.String() != want {
				t.Errorf("stderr = %q, want %q", errOut.String(), want)
			}
			if out.String() != "" {
				t.Errorf("stdout = %q, want empty", out.String())
			}
		})
	}
}

// TestScenariosFileTokenMismatch proves ZING_SCENARIOS_FILE naming a run id
// other than ZING_RUN_TOKEN's own is refused with the exact stderr text
// (PKG9-PLAN.md section 7.3 step 2), even though the file exists and is
// perfectly readable: the path's own token is the check, not whether a read
// would succeed.
func TestScenariosFileTokenMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeScenariosFixture(t, dir, 7, "<scenario></scenario>\n")

	var out, errOut bytes.Buffer
	code := scenarios(scenariosGetenv(map[string]string{
		scenariosTokenEnv: "8", // the run token names a different run than the file's own path
		scenariosFileEnv:  path,
	}), &out, &errOut)

	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if want := scenariosNotJudge + "\n"; errOut.String() != want {
		t.Errorf("stderr = %q, want %q", errOut.String(), want)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

// TestScenariosFileRelative proves a relative ZING_SCENARIOS_FILE is
// refused with the exact "only a judge run" stderr text (PKG9-PLAN.md
// section 7.3 step 2: "must be ... absolute"), even when its own suffix
// would otherwise match the run token.
func TestScenariosFileRelative(t *testing.T) {
	t.Parallel()
	const runID = int64(3)
	relative := filepath.Join("judge", strconv.FormatInt(runID, 10), "scenarios.xml")

	var out, errOut bytes.Buffer
	code := scenarios(scenariosGetenv(map[string]string{
		scenariosTokenEnv: strconv.FormatInt(runID, 10),
		scenariosFileEnv:  relative,
	}), &out, &errOut)

	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if want := scenariosNotJudge + "\n"; errOut.String() != want {
		t.Errorf("stderr = %q, want %q", errOut.String(), want)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

// TestScenariosOpensNoDatabase proves scenarios never touches a database,
// let alone the default one (PKG9-PLAN.md section 7.3: "zing scenarios no
// longer opens the database"): with HOME set to an empty value --
// store.DefaultPath's own os.UserHomeDir call would fail outright, so this
// would fail loudly here too if scenarios still resolved and opened the
// default store path -- the command still reads its file and succeeds. Not
// parallel: it calls t.Setenv on the real process environment.
func TestScenariosOpensNoDatabase(t *testing.T) {
	t.Setenv("HOME", "")

	const runID = int64(9)
	const content = "<scenario id=\"s1\" kind=\"behavior\"><given>g</given><when>w</when><then>t</then></scenario>\n"
	path := writeScenariosFixture(t, t.TempDir(), runID, content)

	var out, errOut bytes.Buffer
	code := scenarios(scenariosGetenv(map[string]string{
		scenariosTokenEnv: strconv.FormatInt(runID, 10),
		scenariosFileEnv:  path,
	}), &out, &errOut)

	if code != 0 {
		t.Errorf("code = %d, want 0 (stderr: %q)", code, errOut.String())
	}
	if out.String() != content {
		t.Errorf("stdout = %q, want %q", out.String(), content)
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want empty", errOut.String())
	}
}
