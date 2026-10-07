// settings_internal_test.go is a whitebox test for buildSettingRows
// (views.go, #81): it lives in package console, not console_test, the
// same views_internal_test.go precedent, because buildSettingRows is
// cleanest proved directly against a synthetic Tuning and stored map
// rather than through a real dispatcher and store.
package console

import (
	"testing"
	"time"

	"zing/internal/dispatch"
)

// TestBuildSettingRows proves buildSettingRows' row order, unit
// conversions, and source line (#81, owner decision Q4): a stored value
// that still matches the live one, with both provenance keys set, reads
// "set by BY at AT"; anything else reads "from zing.toml".
func TestBuildSettingRows(t *testing.T) {
	t.Parallel()

	tune := dispatch.Tuning{MaxParallel: 3, Interval: 30 * time.Second, Budget: 240 * time.Minute}

	t.Run("a matching stored value with both provenance keys shows who and when", func(t *testing.T) {
		t.Parallel()
		stored := map[string]string{
			"dispatch.max_parallel":            "3",
			"dispatch.max_parallel.changed_by": "peter",
			"dispatch.max_parallel.changed_at": "2026-10-06T14:03:00Z",
		}
		rows := buildSettingRows(tune, stored)
		if len(rows) != 3 {
			t.Fatalf("len(rows) = %d, want 3", len(rows))
		}
		want := []struct {
			name   string
			value  int
			source string
		}{
			{dispatch.TuneMaxParallel, 3, "set by peter at 2026-10-06T14:03:00Z"},
			{dispatch.TuneIntervalSeconds, 30, "from zing.toml"},
			{dispatch.TuneAgentMinutes, 240, "from zing.toml"},
		}
		for i, w := range want {
			if rows[i].Name != w.name {
				t.Errorf("rows[%d].Name = %q, want %q", i, rows[i].Name, w.name)
			}
			if rows[i].Value != w.value {
				t.Errorf("rows[%d].Value = %d, want %d", i, rows[i].Value, w.value)
			}
			if rows[i].Source != w.source {
				t.Errorf("rows[%d].Source = %q, want %q", i, rows[i].Source, w.source)
			}
		}
	})

	t.Run("a stored value that differs from the live one shows from zing.toml", func(t *testing.T) {
		t.Parallel()
		stored := map[string]string{
			"dispatch.max_parallel":            "9",
			"dispatch.max_parallel.changed_by": "peter",
			"dispatch.max_parallel.changed_at": "2026-10-06T14:03:00Z",
		}
		rows := buildSettingRows(tune, stored)
		if rows[0].Source != "from zing.toml" {
			t.Errorf("rows[0].Source = %q, want %q", rows[0].Source, "from zing.toml")
		}
	})

	t.Run("a missing changed_at shows from zing.toml", func(t *testing.T) {
		t.Parallel()
		stored := map[string]string{
			"dispatch.max_parallel":            "3",
			"dispatch.max_parallel.changed_by": "peter",
		}
		rows := buildSettingRows(tune, stored)
		if rows[0].Source != "from zing.toml" {
			t.Errorf("rows[0].Source = %q, want %q", rows[0].Source, "from zing.toml")
		}
	})

	t.Run("no stored values at all shows from zing.toml for every row", func(t *testing.T) {
		t.Parallel()
		rows := buildSettingRows(tune, map[string]string{})
		for _, r := range rows {
			if r.Source != "from zing.toml" {
				t.Errorf("row %q Source = %q, want %q", r.Name, r.Source, "from zing.toml")
			}
		}
	})
}
