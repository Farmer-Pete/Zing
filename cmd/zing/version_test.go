package main

import (
	"runtime/debug"
	"testing"
)

// TestVersionString tables versionString's branches: nil build info, a real
// tag, a pseudo-version above a tag (left unchanged), an untagged v0.0.0-
// pseudo-version (routed to vcs.revision, clean and dirty), no settings at
// all, and the (devel)/empty Main.Version shapes a go test or go run binary
// actually carries. These constructed rows are the in-process stand-in for
// the stamped binaries judge scenarios s1, s3, and s4 build for real.
func TestVersionString(t *testing.T) {
	const (
		revision     = "abcdef123456789012345678901234567890ab"
		settingTrue  = "true"
		settingFalse = "false"
	)
	shortRevision := revision[:versionRevisionLen]

	tests := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{
			name: "nil info",
			info: nil,
			want: versionFallback,
		},
		{
			name: "real tag",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v1.2.3"},
				Settings: []debug.BuildSetting{
					{Key: settingVCSRevision, Value: revision},
				},
			},
			want: "v1.2.3",
		},
		{
			name: "pseudo-version above a tag is left unchanged",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v1.2.3-0.20261001120000-" + shortRevision},
				Settings: []debug.BuildSetting{
					{Key: settingVCSRevision, Value: revision},
				},
			},
			want: "v1.2.3-0.20261001120000-" + shortRevision,
		},
		{
			name: "untagged v0.0.0 pseudo-version, clean",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: versionPseudoPrefix + "20261001120000-" + shortRevision},
				Settings: []debug.BuildSetting{
					{Key: settingVCSRevision, Value: revision},
					{Key: settingVCSModified, Value: settingFalse},
				},
			},
			want: shortRevision,
		},
		{
			name: "untagged v0.0.0 pseudo-version, dirty",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: versionPseudoPrefix + "20261001120000-" + shortRevision + "+dirty"},
				Settings: []debug.BuildSetting{
					{Key: settingVCSRevision, Value: revision},
					{Key: settingVCSModified, Value: settingTrue},
				},
			},
			want: shortRevision + versionDirtySuffix,
		},
		{
			name: "v0.0.0 pseudo-version with no settings",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: versionPseudoPrefix + "20261001120000-" + shortRevision},
			},
			want: versionFallback,
		},
		{
			name: "(devel) with revision, clean",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: versionUnset},
				Settings: []debug.BuildSetting{
					{Key: settingVCSRevision, Value: revision},
					{Key: settingVCSModified, Value: settingFalse},
				},
			},
			want: shortRevision,
		},
		{
			name: "(devel) with revision, dirty",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: versionUnset},
				Settings: []debug.BuildSetting{
					{Key: settingVCSRevision, Value: revision},
					{Key: settingVCSModified, Value: settingTrue},
				},
			},
			want: shortRevision + versionDirtySuffix,
		},
		{
			name: "empty Main.Version behaves as (devel)",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: ""},
				Settings: []debug.BuildSetting{
					{Key: settingVCSRevision, Value: revision},
					{Key: settingVCSModified, Value: settingFalse},
				},
			},
			want: shortRevision,
		},
		{
			name: "short revision is returned whole",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: versionUnset},
				Settings: []debug.BuildSetting{
					{Key: settingVCSRevision, Value: "abc123"},
				},
			},
			want: "abc123",
		},
		{
			name: "(devel) with no settings",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: versionUnset},
			},
			want: versionFallback,
		},
		{
			name: "(devel) with vcs.modified but no vcs.revision",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: versionUnset},
				Settings: []debug.BuildSetting{
					{Key: settingVCSModified, Value: settingTrue},
				},
			},
			want: versionFallback,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionString(tt.info); got != tt.want {
				t.Errorf("versionString(%+v) = %q, want %q", tt.info, got, tt.want)
			}
		})
	}
}
