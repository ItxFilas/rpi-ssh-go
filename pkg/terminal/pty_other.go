//go:build !linux

package terminal

import (
	"errors"
	"os"
)

var errPTYUnsupported = errors.New("real local PTY fixture not supported on this platform")

func openPTY() (master, slave *os.File, err error) {
	return nil, nil, errPTYUnsupported
}

func getTerminalEcho(f *os.File) (bool, error) {
	return false, errPTYUnsupported
}
