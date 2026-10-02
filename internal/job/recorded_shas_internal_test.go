package job

import (
	"slices"
	"testing"

	"zing/internal/response"
	"zing/internal/store"
)

// TestRecordedShasSkipsNoOpFix is a regression test for a live ticket: a
// fix that changed nothing landed at the existing HEAD, so its build
// report repeated the previous sha, and every branch check (review, judge,
// ship) then saw "the ticket branch holds commits Zing did not record".
func TestRecordedShasSkipsNoOpFix(t *testing.T) {
	t.Parallel()
	a, b := "aaaa", "bbbb"
	reports := []store.BuildReportRow{
		{Report: response.BuildReport{TaskN: 1, CommitSHA: &a}},
		{Report: response.BuildReport{TaskN: 2, CommitSHA: &b}},
		{Report: response.BuildReport{TaskN: 0}},
		{Report: response.BuildReport{TaskN: 0, CommitSHA: &b}},
	}
	if got, want := recordedShas(reports), []string{a, b}; !slices.Equal(got, want) {
		t.Errorf("recordedShas = %v, want %v", got, want)
	}
}
