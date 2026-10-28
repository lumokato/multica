//go:build windows

package agent

import "os"

// runFakePiCancelHelper is dispatched from TestMain only when the POSIX
// cancellation fixture re-executes the test binary. The fixture is skipped on
// Windows, so this stub exists solely to keep TestMain compilable there.
func runFakePiCancelHelper() {
	os.Exit(2)
}
