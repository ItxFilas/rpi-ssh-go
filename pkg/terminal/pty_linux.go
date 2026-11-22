//go:build linux

package terminal

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var errPTYUnsupported = errors.New("real local PTY fixture not supported on this platform")

func openPTY() (master, slave *os.File, err error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	pno, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		ptmx.Close()
		return nil, nil, fmt.Errorf("ioctl TIOCGPTN: %w", err)
	}

	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		ptmx.Close()
		return nil, nil, fmt.Errorf("ioctl TIOCSPTLCK: %w", err)
	}

	ptsName := fmt.Sprintf("/dev/pts/%d", pno)
	pts, err := os.OpenFile(ptsName, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, nil, fmt.Errorf("open pts %s: %w", ptsName, err)
	}

	return ptmx, pts, nil
}

func getTerminalEcho(f *os.File) (bool, error) {
	termios, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		return false, fmt.Errorf("ioctl TCGETS: %w", err)
	}
	return (termios.Lflag & unix.ECHO) != 0, nil
}
