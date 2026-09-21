package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndex(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	index(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
	if !strings.Contains(rec.Body.String(), "<h1>Zing</h1>") {
		t.Errorf("body missing heading: %q", rec.Body.String())
	}
}
