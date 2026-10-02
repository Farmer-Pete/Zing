// version.go implements "zing version": the build's version, as "zing "
// then the version token, on one stdout line.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

const (
	versionUsage        = "usage: zing version"
	versionPrefix       = "zing "
	versionFallback     = "devel"
	versionUnset        = "(devel)"
	versionPseudoPrefix = "v0.0.0-"
	versionDirtySuffix  = "-dirty"
	versionRevisionLen  = 12
	settingVCSRevision  = "vcs.revision"
	settingVCSModified  = "vcs.modified"
)

// runVersion is "zing version": one stdout line, versionPrefix then the
// running binary's version token, exit 0, nothing on stderr. Any argument
// after the subcommand name is a usage error: versionUsage on stderr,
// nothing on stdout, exit 2, the same code runValidate uses for its usage
// path. ReadBuildInfo returns a nil *BuildInfo whenever ok is false, which
// versionString already maps to the fallback, so the ok flag needs no
// branch of its own.
func runVersion(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, versionUsage)
		return 2
	}
	info, _ := debug.ReadBuildInfo()
	fmt.Fprintln(os.Stdout, versionPrefix+versionString(info))
	return 0
}

// versionString picks the version token from info, in this order:
//  1. info nil (no build info in the binary): versionFallback.
//  2. info.Main.Version set, not versionUnset, and not starting with
//     versionPseudoPrefix: that string, as is. A tagged `go install` puts
//     the tag here. Since Go 1.24 a plain `go build` in an untagged git
//     checkout puts a "v0.0.0-TIME-HASH" pseudo-version here instead,
//     with "+dirty" appended by the toolchain; the owner wants the bare
//     hash for those builds, so that shape is treated as unset and falls
//     through to the vcs.revision branch.
//  3. Else the vcs.revision setting, cut to versionRevisionLen characters
//     when longer, with versionDirtySuffix appended when vcs.modified is
//     the string "true".
//  4. Else versionFallback.
func versionString(info *debug.BuildInfo) string {
	if info == nil {
		return versionFallback
	}
	v := info.Main.Version
	if v != "" && v != versionUnset && !strings.HasPrefix(v, versionPseudoPrefix) {
		return v
	}
	revision := ""
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case settingVCSRevision:
			revision = s.Value
		case settingVCSModified:
			dirty = s.Value == "true"
		}
	}
	if revision == "" {
		return versionFallback
	}
	if len(revision) > versionRevisionLen {
		revision = revision[:versionRevisionLen]
	}
	if dirty {
		revision += versionDirtySuffix
	}
	return revision
}
