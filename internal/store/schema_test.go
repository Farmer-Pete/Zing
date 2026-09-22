package store

import (
	"testing"
)

func TestLoadSchemas_CompilesAll16(t *testing.T) {
	t.Parallel()

	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}
	if got := len(schemas.compiled); got != 16 {
		t.Fatalf("compiled %d schemas, want 16", got)
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
			typ:     "claims",
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
			typ:     "answer",
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
		TicketID: 1, Type: "followup", Author: "you", Body: "any update?",
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
		TicketID: 1, Type: "followup", Author: "you", Body: "x", Payload: []byte(`{"a":1}`),
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
