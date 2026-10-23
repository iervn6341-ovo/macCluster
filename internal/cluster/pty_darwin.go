//go:build darwin

package cluster

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY allocates a pseudo-terminal pair for an interactive `kubectl exec -t`. macOS has no openpty in the Go
// standard library, so it is done with the documented ioctls: open /dev/ptmx, grant and unlock the slave, ask for its name.
func openPTY() (master, slave *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	master = os.NewFile(uintptr(fd), "/dev/ptmx")
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGRANT), 0); e != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("grant pty: %w", e)
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYUNLK), 0); e != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", e)
	}
	var name [128]byte
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("name pty: %w", e)
	}
	n := 0
	for n < len(name) && name[n] != 0 {
		n++
	}
	slave, err = os.OpenFile(string(name[:n]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open pty slave: %w", err)
	}
	return master, slave, nil
}

// setWindowSize applies the client's terminal size. It goes through SyscallConn rather than Fd(): Fd() would put the
// file into blocking mode and race with closing it.
func setWindowSize(f *os.File, width, height uint16) {
	conn, err := f.SyscallConn()
	if err != nil {
		return
	}
	conn.Control(func(fd uintptr) {
		unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: height, Col: width})
	})
}
