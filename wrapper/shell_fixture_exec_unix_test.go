//go:build !windows

package wrapper

import (
	"os"
	"syscall"
)

// execShellFixture replaces the stable test-binary launcher with the stable
// system shell. Replacing rather than parenting the shell preserves the PID,
// process-group, signal, and exit-code behavior the production subprocess
// tests exercise.
func execShellFixture(scriptPath string, originalArgs []string) error {
	args := append([]string{"/bin/sh", scriptPath}, originalArgs...)
	return syscall.Exec("/bin/sh", args, os.Environ()) //nolint:gosec // test-only fixed interpreter
}
