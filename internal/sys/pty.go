package sys

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	tiocGPTN   = 0x80045430
	tiocSPTLCK = 0x40045431
	tiocGWINSZ = 0x5413
	tiocSWINSZ = 0x5414
)

// Winsize is the terminal size, as the kernel represents it.
type Winsize struct {
	Rows, Cols, X, Y uint16
}

// OpenPTY allocates a pseudo-terminal.
//
// Called inside a pod, this comes from the pod's own devpts instance: a pod
// terminal is not a host terminal. The daemon keeps the master, which is what
// lets a session survive the client that started it.
func OpenPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), tiocSPTLCK,
		uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", errno)
	}
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), tiocGPTN,
		uintptr(unsafe.Pointer(&n))); errno != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("pty number: %w", errno)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}

func SetWinsize(fd uintptr, rows, cols int) error {
	ws := Winsize{Rows: uint16(rows), Cols: uint16(cols)}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, tiocSWINSZ,
		uintptr(unsafe.Pointer(&ws))); errno != 0 {
		return errno
	}
	return nil
}

func GetWinsize(fd uintptr) (rows, cols int, err error) {
	var ws Winsize
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, tiocGWINSZ,
		uintptr(unsafe.Pointer(&ws))); errno != 0 {
		return 0, 0, errno
	}
	return int(ws.Rows), int(ws.Cols), nil
}

// IsTTY reports whether a descriptor is a terminal. vibepod uses this to
// decide whether a routed command needs a PTY at all: agents parse stdout and
// stderr separately, and a PTY would merge them.
func IsTTY(fd uintptr) bool {
	var t [64]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS,
		uintptr(unsafe.Pointer(&t[0])))
	return errno == 0
}
