package orchestrator

import (
	"os"
	"testing"
)

// TestMain points git at an empty global and system config for every test
// in this package. FilterDrivers reads every config level on purpose (an
// owner's global driver is exactly what the hardening is for), so a host
// that installed git-lfs globally, as the GitHub runner does, would
// otherwise make the driver tests see a "lfs" driver nobody configured.
// Each git child of the tests and of the orchestrator under test inherits
// these two variables.
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	os.Exit(m.Run())
}
