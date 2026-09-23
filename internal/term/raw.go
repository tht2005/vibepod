package term

import (
	"os"
	"syscall"
	"unsafe"
)

// DetachKey is Ctrl-\ . It detaches from a session and leaves it running.
// Deliberately not a prefix key: an agent running inside the pod has its own
// keybindings, and a two-key prefix would collide with them.
const DetachKey = 0x1c

type State struct {
	termios syscall.Termios
	fd      uintptr
}

// MakeRaw hands the terminal to whatever is on the other end, so a full-screen
// program in the pod behaves exactly as it would locally.
func MakeRaw(fd uintptr) (*State, error) {
	var t syscall.Termios
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	saved := t
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG |
		syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, syscall.TCSETS, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	return &State{termios: saved, fd: fd}, nil
}

// Restore puts the terminal back. Every path out of an attach must reach this,
// including a panic, or the user is left with a terminal that does not echo.
func (s *State) Restore() {
	if s == nil {
		return
	}
	t := s.termios
	_ = ioctl(s.fd, syscall.TCSETS, unsafe.Pointer(&t))
}

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// IsTTY reports whether a file is a terminal.
func IsTTY(f *os.File) bool {
	var t syscall.Termios
	return ioctl(f.Fd(), syscall.TCGETS, unsafe.Pointer(&t)) == nil
}
