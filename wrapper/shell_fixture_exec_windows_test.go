//go:build windows

package wrapper

import "errors"

func execShellFixture(string, []string) error {
	return errors.New("POSIX shell fixtures are unavailable on Windows")
}
