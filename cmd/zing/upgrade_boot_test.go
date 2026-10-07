package main

import "testing"

func TestDecideBoot(t *testing.T) {
	const (
		to7      = "0123456789abcdef"
		running7 = "0123456"
	)

	tests := []struct {
		name    string
		marker  upgradeMarker
		found   bool
		running string
		want    bootAction
	}{
		{
			name:    "missing",
			found:   false,
			running: running7,
			want:    bootNormal,
		},
		{
			name:    "pending match",
			marker:  upgradeMarker{State: markerPending, ToSHA: to7},
			found:   true,
			running: running7,
			want:    bootWatch,
		},
		{
			name:    "pending mismatch",
			marker:  upgradeMarker{State: markerPending, ToSHA: to7},
			found:   true,
			running: "fedcba9876543210",
			want:    bootDiscard,
		},
		{
			name:    "pending mismatch running devel",
			marker:  upgradeMarker{State: markerPending, ToSHA: to7},
			found:   true,
			running: versionFallback,
			want:    bootDiscard,
		},
		{
			name:    "pending mismatch running dirty",
			marker:  upgradeMarker{State: markerPending, ToSHA: to7},
			found:   true,
			running: "0123456" + versionDirtySuffix,
			want:    bootDiscard,
		},
		{
			name:    "pending mismatch running 6-char prefix",
			marker:  upgradeMarker{State: markerPending, ToSHA: to7},
			found:   true,
			running: "012345",
			want:    bootDiscard,
		},
		{
			name:    "attempted match",
			marker:  upgradeMarker{State: markerAttempted, ToSHA: to7},
			found:   true,
			running: running7,
			want:    bootRollback,
		},
		{
			name:    "attempted mismatch",
			marker:  upgradeMarker{State: markerAttempted, ToSHA: to7},
			found:   true,
			running: "fedcba9876543210",
			want:    bootDiscard,
		},
		{
			name:    "attempted mismatch running devel",
			marker:  upgradeMarker{State: markerAttempted, ToSHA: to7},
			found:   true,
			running: versionFallback,
			want:    bootDiscard,
		},
		{
			name:    "attempted mismatch running dirty",
			marker:  upgradeMarker{State: markerAttempted, ToSHA: to7},
			found:   true,
			running: "0123456" + versionDirtySuffix,
			want:    bootDiscard,
		},
		{
			name:    "attempted mismatch running 6-char prefix",
			marker:  upgradeMarker{State: markerAttempted, ToSHA: to7},
			found:   true,
			running: "012345",
			want:    bootDiscard,
		},
		{
			name:    "rolled_back match",
			marker:  upgradeMarker{State: markerRolledBack, ToSHA: to7},
			found:   true,
			running: running7,
			want:    bootReport,
		},
		{
			name:    "rolled_back mismatch",
			marker:  upgradeMarker{State: markerRolledBack, ToSHA: to7},
			found:   true,
			running: "fedcba9876543210",
			want:    bootReport,
		},
		{
			name:    "bogus state",
			marker:  upgradeMarker{State: "bogus", ToSHA: to7},
			found:   true,
			running: running7,
			want:    bootDiscard,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideBoot(tt.marker, tt.found, tt.running); got != tt.want {
				t.Errorf("decideBoot(%+v, %v, %q) = %v, want %v", tt.marker, tt.found, tt.running, got, tt.want)
			}
		})
	}
}

func TestBootOutcome(t *testing.T) {
	for _, watching := range []bool{false, true} {
		for _, booted := range []bool{false, true} {
			for _, deadlinePassed := range []bool{false, true} {
				for _, signalled := range []bool{false, true} {
					var wantOutcome, wantCause string
					switch {
					case !watching || booted:
						wantOutcome, wantCause = outcomeNone, ""
					case signalled:
						wantOutcome, wantCause = outcomeRevert, ""
					case deadlinePassed:
						wantOutcome, wantCause = outcomeFailed, bootCauseDeadline
					default:
						wantOutcome, wantCause = outcomeFailed, bootCauseStopped
					}

					gotOutcome, gotCause := bootOutcome(watching, booted, deadlinePassed, signalled)
					if gotOutcome != wantOutcome || gotCause != wantCause {
						t.Errorf("bootOutcome(%v, %v, %v, %v) = (%v, %v), want (%v, %v)",
							watching, booted, deadlinePassed, signalled, gotOutcome, gotCause, wantOutcome, wantCause)
					}
				}
			}
		}
	}
}
