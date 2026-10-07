package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"zing/internal/response"
	"zing/internal/schemagen"
)

// testFortyHexSHA is a stand-in 40-character hex commit SHA, reused across
// this file's Package 9 schema tests (finding, verdict, and respond
// artifacts each require one).
const testFortyHexSHA = "0123456789abcdef0123456789abcdef01234567"

func TestLoadSchemas_CompilesAllRegistered(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}
	want := len(schemagen.Registry())
	if got := len(schemas.compiled); got != want {
		t.Fatalf("compiled %d schemas, want %d (len(schemagen.Registry()))", got, want)
	}
}

func TestValidate_CommittedExamplesPass(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.ValidateExamples(); err != nil {
		t.Errorf("ValidateExamples: %v", err)
	}
}

func TestValidate_BadPayloads(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	tests := []struct {
		name    string
		table   string
		typ     string
		payload string
		want    string
	}{
		{
			name:    "missing required field",
			table:   testTableMessages,
			typ:     testTypeQuestion,
			payload: `{"kind":"question","state":"open","recommended":"x","options":[]}`,
			want:    "payload does not match schema question: /: missing property 'key'",
		},
		{
			name:    "bad enum value",
			table:   testTableMessages,
			typ:     testTypeQuestion,
			payload: `{"key":"Q1","kind":"bogus","state":"open","recommended":"x","options":[]}`,
			want:    "payload does not match schema question: /kind: value must be one of 'question', 'gate', 'split', 'perimeter', 'review', 'merge'",
		},
		{
			name:    "minItems violation",
			table:   testTableArtifacts,
			typ:     testTypeClaims,
			payload: `[]`,
			want:    "payload does not match schema claims: /: minItems: got 0, want 1",
		},
		{
			name:    "wrong type",
			table:   testTableMessages,
			typ:     testTypeQuestion,
			payload: `{"key":123,"kind":"question","state":"open","recommended":"x","options":[]}`,
			want:    "payload does not match schema question: /key: got number, want string",
		},
		{
			name:    "two invalid properties picks the first by path",
			table:   testTableMessages,
			typ:     testTypeQuestion,
			payload: `{"key":"bad","kind":"bogus","state":"open","recommended":"x","options":[]}`,
			want:    "payload does not match schema question: /key: 'bad' does not match pattern '^Q[0-9]+$'",
		},
		{
			name:    "instance location with slash and tilde is escaped per RFC 6901",
			table:   testTableMessages,
			typ:     testTypeAnswer,
			payload: `{"option":"a","items":{"a/b~c":"bogus"}}`,
			want:    "payload does not match schema answer: /items/a~1b~0c: value must be one of 'accept', 'reject', 'drop', 'discuss'",
		},
		{
			name:    "children array under length rejects with minItems",
			table:   testTableArtifacts,
			typ:     "children",
			payload: `{"children":[{"key":"c1","title":"t","body":"b","depends_on":[]}],"notes":""}`,
			want:    "payload does not match schema children: /children: minItems: got 1, want 2",
		},
		{
			name:    "unknown table/type pair",
			table:   "bogus",
			typ:     "type",
			payload: `{}`,
			want:    "payload does not match schema type: /: no schema for bogus/type",
		},
		{
			name:    "invalid JSON",
			table:   testTableMessages,
			typ:     testTypeQuestion,
			payload: `{not json`,
			want:    "payload does not match schema question: /: invalid JSON: invalid character 'n' looking for beginning of object key string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := schemas.validate(tt.table, tt.typ, []byte(tt.payload))
			if err == nil {
				t.Fatalf("validate(%s, %s, %s) = nil, want error", tt.table, tt.typ, tt.payload)
			}
			if err.Error() != tt.want {
				t.Errorf("validate(%s, %s, %s) = %q, want %q", tt.table, tt.typ, tt.payload, err.Error(), tt.want)
			}
		})
	}
}

// TestValidate_FailuresWrapErrSchemaInvalid proves every validate failure
// path (design section "store": ErrSchemaInvalid) satisfies
// errors.Is(err, ErrSchemaInvalid) while keeping validate's exact message
// text unchanged (#213's dispatcher fix depends on both: the sentinel to
// recognize a schema failure, the text to show the owner what failed). The
// invalid-JSON case also satisfies errors.As for the underlying
// *json.SyntaxError, since schemaInvalidError.Unwrap returns the cause too.
func TestValidate_FailuresWrapErrSchemaInvalid(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	tests := []struct {
		name       string
		table      string
		typ        string
		payload    string
		want       string
		wantSyntax bool
	}{
		{
			name:    "unknown type",
			table:   testTableMessages,
			typ:     "nope",
			payload: `{}`,
			want:    "payload does not match schema nope: /: no schema for messages/nope",
		},
		{
			name:       "invalid JSON",
			table:      testTableMessages,
			typ:        testTypeQuestion,
			payload:    `{not json`,
			want:       "payload does not match schema question: /: invalid JSON: invalid character 'n' looking for beginning of object key string",
			wantSyntax: true,
		},
		{
			name:    "options null",
			table:   testTableMessages,
			typ:     testTypeQuestion,
			payload: `{"key":"Q1","kind":"question","state":"open","recommended":"a","options":null}`,
			want:    "payload does not match schema question: /options: got null, want array",
		},
		{
			name:    "five option array over maxItems",
			table:   testTableMessages,
			typ:     testTypeQuestion,
			payload: `{"key":"Q1","kind":"question","state":"open","recommended":"a","options":[{"key":"a","text":"a"},{"key":"b","text":"b"},{"key":"c","text":"c"},{"key":"d","text":"d"},{"key":"e","text":"e"}]}`,
			want:    "payload does not match schema question: /options: maxItems: got 5, want 4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := schemas.validate(tt.table, tt.typ, []byte(tt.payload))
			if err == nil {
				t.Fatalf("validate(%s, %s, %s) = nil, want error", tt.table, tt.typ, tt.payload)
			}
			if !errors.Is(err, ErrSchemaInvalid) {
				t.Errorf("validate(%s, %s, %s): errors.Is(err, ErrSchemaInvalid) = false, want true", tt.table, tt.typ, tt.payload)
			}
			if err.Error() != tt.want {
				t.Errorf("validate(%s, %s, %s) = %q, want %q", tt.table, tt.typ, tt.payload, err.Error(), tt.want)
			}
			if tt.wantSyntax {
				if syntaxErr, ok := errors.AsType[*json.SyntaxError](err); !ok || syntaxErr == nil {
					t.Errorf("validate(%s, %s, %s): errors.AsType[*json.SyntaxError](err) = %v, %v, want non-nil, true", tt.table, tt.typ, tt.payload, syntaxErr, ok)
				}
			}
		})
	}
}

// TestClaimsAndChildren_EmptyArrayIsTheOnlyValidEmptyShape asserts the
// section 6.3 data-model choice: a required array field without omitempty
// must be present and must be an array. "[]" validates; null and an absent
// key each fail.
func TestClaimsAndChildren_EmptyArrayIsTheOnlyValidEmptyShape(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	// claims has minItems=1, so an empty array is itself invalid (covered
	// above); this test only exercises fields without a minItems floor.
	tests := []struct {
		name    string
		table   string
		typ     string
		payload string
		wantOK  bool
	}{
		{"empty array validates", testTableArtifacts, testTypePlanreview, `{"findings":[]}`, true},
		{"null fails", testTableArtifacts, testTypePlanreview, `{"findings":null}`, false},
		{"absent key fails", testTableArtifacts, testTypePlanreview, `{}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := schemas.validate(tt.table, tt.typ, []byte(tt.payload))
			if tt.wantOK && err != nil {
				t.Errorf("validate(%s) = %v, want nil", tt.payload, err)
			}
			if !tt.wantOK && err == nil {
				t.Errorf("validate(%s) = nil, want error", tt.payload)
			}
		})
	}
}

// TestValidate_OptionAndCommitSHAAreOptional proves Fix 1: an answer payload
// with no "option" key, and a task artifact payload with no "commit_sha"
// key, both validate. Before the omitempty fix, the missing key failed as a
// missing required property.
func TestValidate_OptionAndCommitSHAAreOptional(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	t.Run("answer with no option validates", func(t *testing.T) {
		t.Parallel()
		if err := schemas.validate(testTableMessages, testTypeAnswer, []byte(`{}`)); err != nil {
			t.Errorf("validate(answer, no option) = %v, want nil", err)
		}
	})

	t.Run("answer with option a validates", func(t *testing.T) {
		t.Parallel()
		if err := schemas.validate(testTableMessages, testTypeAnswer, []byte(`{"option":"a"}`)); err != nil {
			t.Errorf("validate(answer, option a) = %v, want nil", err)
		}
	})

	t.Run("answer with two-char option fails the pattern", func(t *testing.T) {
		t.Parallel()
		err := schemas.validate(testTableMessages, testTypeAnswer, []byte(`{"option":"ab"}`))
		if err == nil {
			t.Fatal("validate(answer, option ab) = nil, want error")
		}
		want := "payload does not match schema answer: /option: 'ab' does not match pattern '^[a-z]$'"
		if err.Error() != want {
			t.Errorf("validate(answer, option ab) = %q, want %q", err.Error(), want)
		}
	})

	taskPayload := func(extra string) string {
		return `{"n":1,"test":"t","demo":true,"text":"x","title":"y","state":"pending"` + extra + `}`
	}

	t.Run("task with no commit_sha validates", func(t *testing.T) {
		t.Parallel()
		if err := schemas.validate(testTableArtifacts, "task", []byte(taskPayload(""))); err != nil {
			t.Errorf("validate(task, no commit_sha) = %v, want nil", err)
		}
	})

	t.Run("task with 40-hex commit_sha validates", func(t *testing.T) {
		t.Parallel()
		sha := `,"commit_sha":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"`
		if err := schemas.validate(testTableArtifacts, "task", []byte(taskPayload(sha))); err != nil {
			t.Errorf("validate(task, 40-hex commit_sha) = %v, want nil", err)
		}
	})

	t.Run("task with short commit_sha fails", func(t *testing.T) {
		t.Parallel()
		short := `,"commit_sha":"deadbeef"`
		err := schemas.validate(testTableArtifacts, "task", []byte(taskPayload(short)))
		if err == nil {
			t.Fatal("validate(task, short commit_sha) = nil, want error")
		}
		want := "payload does not match schema task: /commit_sha: 'deadbeef' does not match pattern '^[0-9a-f]{40}$'"
		if err.Error() != want {
			t.Errorf("validate(task, short commit_sha) = %q, want %q", err.Error(), want)
		}
	})
}

func TestInsertMessage_PayloadRules(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedProjectAndTicket(t, s)

	// A payload-less type (followup) inserts with NULL payload.
	id, err := s.InsertMessage(ctx, Message{
		TicketID: 1, Type: "followup", Author: testAuthorYou, Body: "any update?",
	})
	if err != nil {
		t.Fatalf("InsertMessage(followup, no payload): %v", err)
	}
	var payload *string
	if err = s.db.QueryRowContext(ctx, "SELECT payload FROM messages WHERE id = ?", id).Scan(&payload); err != nil {
		t.Fatalf("query payload: %v", err)
	}
	if payload != nil {
		t.Errorf("followup payload = %q, want NULL", *payload)
	}

	// A payload-less type rejects a supplied payload.
	_, err = s.InsertMessage(ctx, Message{
		TicketID: 1, Type: "followup", Author: testAuthorYou, Body: "x", Payload: []byte(`{"a":1}`),
	})
	wantErr := "message type followup takes no payload"
	if err == nil || err.Error() != wantErr {
		t.Errorf("InsertMessage(followup, with payload) = %v, want %q", err, wantErr)
	}

	// A payload-carrying type requires a payload.
	_, err = s.InsertMessage(ctx, Message{
		TicketID: 1, Type: testTypeState, Author: testAuthorZing,
	})
	if err == nil {
		t.Error("InsertMessage(state, no payload) = nil, want error")
	}
}

// dbPath returns a fresh temp-file database path for t.
func dbPath(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/zing.db"
}

// escalationPayloadJSON builds a schema-shaped escalation payload with the
// given code and origin, so TestValidate_EscalationCodeAndOrigin can swap
// just the field under test.
func escalationPayloadJSON(code, origin string) []byte {
	return []byte(fmt.Sprintf(
		`{"code":%q,"what":"w","why":"y","tried":"t","options":["retry"],"origin":%q}`, code, origin))
}

// TestValidate_EscalationCodeAndOrigin proves every one of the seventeen
// EscalationCode values (fourteen, plus Package 8's sandbox_unavailable and
// replan_unsupported, plus Package 9's pr_closed) and the eighteen
// EscalationOrigin values validates against the committed escalation
// schema, and an unknown value of either fails (design section 6.7, task
// 4c; design section 4.1, Packages 8 and 9).
func TestValidate_EscalationCodeAndOrigin(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	codes := response.EscalationCode("").Values()
	if len(codes) != 17 {
		t.Fatalf("len(EscalationCode values) = %d, want 17", len(codes))
	}
	for _, code := range codes {
		t.Run("code "+code, func(t *testing.T) {
			t.Parallel()
			payload := escalationPayloadJSON(code, string(response.EscalationOriginSeal))
			if err := schemas.validate(testTableMessages, testTypeEscalation, payload); err != nil {
				t.Errorf("validate(code=%s): %v, want nil", code, err)
			}
		})
	}

	origins := response.EscalationOrigin("").Values()
	if len(origins) != 18 {
		t.Fatalf("len(EscalationOrigin values) = %d, want 18", len(origins))
	}
	for _, origin := range origins {
		t.Run("origin "+origin, func(t *testing.T) {
			t.Parallel()
			payload := escalationPayloadJSON(string(response.EscalationCodeOther), origin)
			if err := schemas.validate(testTableMessages, testTypeEscalation, payload); err != nil {
				t.Errorf("validate(origin=%s): %v, want nil", origin, err)
			}
		})
	}

	t.Run("unknown code fails", func(t *testing.T) {
		t.Parallel()
		payload := escalationPayloadJSON("bogus", string(response.EscalationOriginSeal))
		if err := schemas.validate(testTableMessages, testTypeEscalation, payload); err == nil {
			t.Error("validate(unknown code) = nil, want error")
		}
	})

	t.Run("unknown origin fails", func(t *testing.T) {
		t.Parallel()
		payload := escalationPayloadJSON(string(response.EscalationCodeOther), "bogus")
		if err := schemas.validate(testTableMessages, testTypeEscalation, payload); err == nil {
			t.Error("validate(unknown origin) = nil, want error")
		}
	})
}

// TestValidate_EscalationSessionIDOptional proves session_id may be absent
// (design section 6.7): an escalation payload with no session_id key
// validates, and one with an integer session_id also validates.
func TestValidate_EscalationSessionIDOptional(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	noSessionID := `{"code":"other","what":"w","why":"y","tried":"t","options":["retry"],"origin":"seal"}`
	if err := schemas.validate(testTableMessages, testTypeEscalation, []byte(noSessionID)); err != nil {
		t.Errorf("validate(no session_id): %v, want nil", err)
	}

	withSessionID := `{"code":"other","what":"w","why":"y","tried":"t","options":["retry"],"origin":"seal","session_id":42}`
	if err := schemas.validate(testTableMessages, testTypeEscalation, []byte(withSessionID)); err != nil {
		t.Errorf("validate(session_id=42): %v, want nil", err)
	}
}

// TestFileDecisionSchemaIsNarrow proves the file artifact's decision enum
// holds only accept and reject (section 4.1): FileArtifact.Decision's own
// type narrows the four-value Decision enum tag on Package 3's field, which
// generated a six-entry enum by appending instead of narrowing.
func TestFileDecisionSchemaIsNarrow(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	filePayload := func(decision string) string {
		return `{"path":"a.go","action":"create","reason":"r","trust_root":false,"style_guide":false,"task_n":1,"decision":"` + decision + `"}`
	}

	tests := []struct {
		name     string
		decision string
		wantOK   bool
	}{
		{"accept validates", "accept", true},
		{"reject validates", "reject", true},
		{"drop is refused", "drop", false},
		{"discuss is refused", "discuss", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := schemas.validate(testTableArtifacts, "file", []byte(filePayload(tt.decision)))
			if tt.wantOK && err != nil {
				t.Errorf("validate(decision=%s) = %v, want nil", tt.decision, err)
			}
			if !tt.wantOK && err == nil {
				t.Errorf("validate(decision=%s) = nil, want error", tt.decision)
			}
		})
	}
}

// findingArtifactPayload builds a schema-shaped finding artifact payload
// with the given id and (when non-empty) decision, so this file's finding
// artifact tests can each swap just the field under test.
func findingArtifactPayload(id, decision string) string {
	extra := ""
	if decision != "" {
		extra = `,"decision":"` + decision + `"`
	}
	return `{"lens":"tests","severity":"minor","location":"a.go:1","text":"t","fix":"f",` +
		`"id":"` + id + `","round":1,"sha":"` + testFortyHexSHA + `","lenses":["tests"]` + extra + `}`
}

// TestFindingArtifactSchema proves the finding artifact schema's decision
// enum is FindingDecision's own three values, narrower than the four-value
// Decision enum tag on the wire Finding type would otherwise produce
// (section 4.1, the same narrowing TestFileDecisionSchemaIsNarrow proves
// for the file artifact), and that both an ordinary round id (r1f1) and a
// held row's id (r1h2) validate.
func TestFindingArtifactSchema(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	const findingID = "r1f1"
	tests := []struct {
		name     string
		id       string
		decision string
		wantOK   bool
	}{
		{"accept validates", findingID, string(response.FindingAccept), true},
		{"drop validates", findingID, string(response.FindingDrop), true},
		{"discuss validates", findingID, string(response.FindingDiscuss), true},
		{"reject is refused", findingID, "reject", false},
		{"a held r1h2 id passes", "r1h2", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := findingArtifactPayload(tt.id, tt.decision)
			err := schemas.validate(testTableArtifacts, "finding", []byte(payload))
			if tt.wantOK && err != nil {
				t.Errorf("validate(id=%s, decision=%s) = %v, want nil", tt.id, tt.decision, err)
			}
			if !tt.wantOK && err == nil {
				t.Errorf("validate(id=%s, decision=%s) = nil, want error", tt.id, tt.decision)
			}
		})
	}
}

// TestFindingArtifactOwnerPicked proves the finding artifact schema's own
// owner_picked field (ticket 55) is an optional boolean: an explicit true
// validates, the key's absence (every row stored before this field) still
// validates, and a non-boolean value is refused.
func TestFindingArtifactOwnerPicked(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	base := findingArtifactPayload("r1f1", string(response.FindingAccept))
	trimmed := strings.TrimSuffix(base, "}")

	tests := []struct {
		name    string
		payload string
		wantOK  bool
	}{
		{"explicit owner_picked true validates", trimmed + `,"owner_picked":true}`, true},
		{"no owner_picked key validates", base, true},
		{"non-bool owner_picked is refused", trimmed + `,"owner_picked":"yes"}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := schemas.validate(testTableArtifacts, "finding", []byte(tt.payload))
			if tt.wantOK && err != nil {
				t.Errorf("validate(%s) = %v, want nil", tt.name, err)
			}
			if !tt.wantOK && err == nil {
				t.Errorf("validate(%s) = nil, want error", tt.name)
			}
		})
	}
}

// TestFindingDecisionValues proves response.FindingDecision's own Values()
// holds exactly accept, drop, discuss, in that order (section 4.1): reject
// cannot be stored, unlike the owner's triage Decision type.
func TestFindingDecisionValues(t *testing.T) {
	t.Parallel()

	want := []string{string(response.FindingAccept), string(response.FindingDrop), string(response.FindingDiscuss)}
	got := response.FindingDecision("").Values()
	if len(got) != len(want) {
		t.Fatalf("FindingDecision.Values() = %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("FindingDecision.Values()[%d] = %q, want %q", i, got[i], v)
		}
	}
}

// TestRound21Validates proves a finding row and a verdict row with round 21
// still pass their schemas (section 4.1): Round has no maximum, only the
// agent budget bounds it.
func TestRound21Validates(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	finding := `{"lens":"tests","severity":"minor","location":"a.go:1","text":"t","fix":"f",` +
		`"id":"r21f1","round":21,"sha":"` + testFortyHexSHA + `","lenses":["tests"]}`
	if err := schemas.validate(testTableArtifacts, "finding", []byte(finding)); err != nil {
		t.Errorf("validate(finding, round=21) = %v, want nil", err)
	}

	verdict := `{"scenario_id":"s1","result":"pass","evidence":"e","kind":"behavior","round":21,"sha":"` +
		testFortyHexSHA + `"}`
	if err := schemas.validate(testTableArtifacts, "verdict", []byte(verdict)); err != nil {
		t.Errorf("validate(verdict, round=21) = %v, want nil", err)
	}
}

// TestVerdictArtifactSchema proves the verdict artifact schema requires
// kind, round, and sha alongside the embedded Verdict's scenario_id,
// result, and evidence (section 4.1), and that the optional check_exit
// (set only on a row a check re-run wrote) may be -1 (timed out) or absent.
func TestVerdictArtifactSchema(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	base := `{"scenario_id":"s1","result":"pass","evidence":"e","kind":"behavior","round":1,"sha":"` +
		testFortyHexSHA + `"}`
	if err := schemas.validate(testTableArtifacts, "verdict", []byte(base)); err != nil {
		t.Errorf("validate(no check_exit) = %v, want nil", err)
	}

	withCheckExit := `{"scenario_id":"s1","result":"pass","evidence":"e","kind":"behavior","round":1,"sha":"` +
		testFortyHexSHA + `","check_exit":-1}`
	if err := schemas.validate(testTableArtifacts, "verdict", []byte(withCheckExit)); err != nil {
		t.Errorf("validate(check_exit=-1) = %v, want nil", err)
	}

	missingKind := `{"scenario_id":"s1","result":"pass","evidence":"e","round":1,"sha":"` + testFortyHexSHA + `"}`
	if err := schemas.validate(testTableArtifacts, "verdict", []byte(missingKind)); err == nil {
		t.Error("validate(no kind) = nil, want error")
	}
}

// respondThreadsJSON builds n schema-shaped, uniquely-id'd thread actions
// as a JSON array, for TestRespondArtifact101Threads.
func respondThreadsJSON(n int) string {
	threads := make([]string, n)
	for i := range n {
		threads[i] = fmt.Sprintf(`{"id":"t%d","action":"reply","text":"ok"}`, i+1)
	}
	return "[" + strings.Join(threads, ",") + "]"
}

// respondArtifactPayload builds a schema-shaped respond artifact payload
// with n threads and one seen entry, so this file's respond artifact tests
// can each swap just the field under test.
func respondArtifactPayload(n int) string {
	return fmt.Sprintf(
		`{"threads":%s,"batch":1,"sha":%q,"seen":[{"tid":"t0123456789abcdef","last_comment":%q}]}`,
		respondThreadsJSON(n), testFortyHexSHA, strings.Repeat("0123456789abcdef", 4),
	)
}

// TestRespondArtifact101Threads proves the respond artifact's threads array
// keeps its minItems=1,maxItems=1000 bound (section 4.1, the same bound
// ListThreads enforces on a POLL batch, design section 10.4): 101 and 1000
// threads both validate, 1001 is refused.
func TestRespondArtifact101Threads(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	tests := []struct {
		n      int
		wantOK bool
	}{
		{101, true},
		{1000, true},
		{1001, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d threads", tt.n), func(t *testing.T) {
			t.Parallel()
			payload := respondArtifactPayload(tt.n)
			err := schemas.validate(testTableArtifacts, "respond", []byte(payload))
			if tt.wantOK && err != nil {
				t.Errorf("validate(%d threads) = %v, want nil", tt.n, err)
			}
			if !tt.wantOK && err == nil {
				t.Errorf("validate(%d threads) = nil, want error", tt.n)
			}
		})
	}
}

// TestRespondArtifactSeenRequired proves seen is a required array, not
// omitempty (section 4.1): it is code-read, never model output, so a
// respond artifact row always carries it, and a payload missing the key
// entirely is refused the same way a missing threads, batch, or sha is.
func TestRespondArtifactSeenRequired(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	missingSeen := `{"threads":[{"id":"t1","action":"reply","text":"ok"}],"batch":1,"sha":"` + testFortyHexSHA + `"}`
	if err := schemas.validate(testTableArtifacts, "respond", []byte(missingSeen)); err == nil {
		t.Error("validate(no seen) = nil, want error")
	}

	withSeen := respondArtifactPayload(1)
	if err := schemas.validate(testTableArtifacts, "respond", []byte(withSeen)); err != nil {
		t.Errorf("validate(with seen) = %v, want nil", err)
	}
}

// TestRespondArtifactSchema proves the respond artifact schema requires
// threads, batch, and sha alongside seen (section 4.1), each refused on its
// own when missing, and that a fully populated row validates.
func TestRespondArtifactSchema(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	if err := schemas.validate(testTableArtifacts, "respond", []byte(respondArtifactPayload(1))); err != nil {
		t.Errorf("validate(full payload) = %v, want nil", err)
	}

	seen := `[{"tid":"t0123456789abcdef","last_comment":"` + strings.Repeat("0123456789abcdef", 4) + `"}]`
	tests := []struct {
		name    string
		payload string
	}{
		{"missing threads", fmt.Sprintf(`{"batch":1,"sha":%q,"seen":%s}`, testFortyHexSHA, seen)},
		{"missing batch", fmt.Sprintf(`{"threads":%s,"sha":%q,"seen":%s}`, respondThreadsJSON(1), testFortyHexSHA, seen)},
		{"missing sha", fmt.Sprintf(`{"threads":%s,"batch":1,"seen":%s}`, respondThreadsJSON(1), seen)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := schemas.validate(testTableArtifacts, "respond", []byte(tt.payload)); err == nil {
				t.Errorf("validate(%s) = nil, want error", tt.name)
			}
		})
	}
}
