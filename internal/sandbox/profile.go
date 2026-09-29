package sandbox

import (
	"errors"
	"fmt"
	"strings"
)

// The two placeholder lines sandbox/build.sb carries (section 5.1), replaced
// exactly once each at render time.
const (
	readPathsPlaceholder   = ";;READ_PATHS;;"
	consoleDenyPlaceholder = ";;CONSOLE_DENY;;"
)

// renderProfile replaces base's two placeholder lines: readPathsPlaceholder
// with one "(allow file-read-data (subpath "<path>"))" line per readPaths
// entry (or nothing, for an empty list), and consoleDenyPlaceholder with the
// console-port deny rule. A port outside 1-65535 is an error, so Load can
// record the sandbox unavailable with reason "profile rejected" rather than
// silently rendering an invalid profile (section 5.1).
func renderProfile(base []byte, readPaths []string, consolePort int) (string, error) {
	deny, err := renderConsoleDeny(consolePort)
	if err != nil {
		return "", err
	}
	text := string(base)
	text = strings.Replace(text, readPathsPlaceholder, renderReadPaths(readPaths), 1)
	text = strings.Replace(text, consoleDenyPlaceholder, deny, 1)
	return text, nil
}

// renderReadPaths renders one "(allow file-read-data (subpath "<path>"))"
// line per entry in paths, newline-joined, or "" for an empty list (section
// 5.1's own wording: "or by nothing").
func renderReadPaths(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	lines := make([]string, len(paths))
	for i, p := range paths {
		lines[i] = fmt.Sprintf(`(allow file-read-data (subpath %q))`, p)
	}
	return strings.Join(lines, "\n")
}

// errBadConsolePort is renderConsoleDeny's own sentinel: a port outside 1 to
// 65535 (section 5.1).
var errBadConsolePort = errors.New("sandbox: console port out of range")

// renderConsoleDeny renders the console-port deny rule (section 5.1): the
// host is always "*", because the console binds loopback and the tailnet
// address and seatbelt accepts only "*" or "localhost" as a host.
func renderConsoleDeny(port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", errBadConsolePort
	}
	return fmt.Sprintf(`(deny network-outbound (remote tcp "*:%d"))`, port), nil
}
