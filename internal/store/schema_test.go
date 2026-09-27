package store

import (
	"fmt"
	"testing"

	"zing/internal/response"
	"zing/internal/schemagen"
)

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
			name:    "root array under length rejects with root minItems",
			table:   testTableArtifacts,
			typ:     "children",
			payload: `[{"key":"c1","title":"t","body":"b","depends_on":[]}]`,
			want:    "payload does not match schema children: /: minItems: got 1, want 2",
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

// TestValidate_EscalationCodeAndOrigin proves every one of the thirteen
// EscalationCode values and the eleven EscalationOrigin values validates
// against the committed escalation schema, and an unknown value of either
// fails (design section 6.7, task 4c).
func TestValidate_EscalationCodeAndOrigin(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	codes := response.EscalationCode("").Values()
	if len(codes) != 13 {
		t.Fatalf("len(EscalationCode values) = %d, want 13", len(codes))
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
	if len(origins) != 11 {
		t.Fatalf("len(EscalationOrigin values) = %d, want 11", len(origins))
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
