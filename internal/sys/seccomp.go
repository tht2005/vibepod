//go:build linux && amd64

package sys

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// The exec gate: a seccomp filter that traps execve/execveat as a user
// notification, so a supervisor outside the pod sees every program launch.

const (
	sysSeccomp = 317

	seccompSetModeFilter = 1
	seccompGetNotifSizes = 3

	filterFlagTSync       = 1 << 0
	filterFlagNewListener = 1 << 3
	filterFlagTSyncESRCH  = 1 << 4

	retAllow      = 0x7fff0000
	retUserNotif  = 0x7fc00000
	auditArchX864 = 0xC000003E

	ioctlNotifRecv    = 0xC0502100
	ioctlNotifSend    = 0xC0182101
	ioctlNotifIDValid = 0x40082102

	// NotifRespContinue lets the syscall proceed as if it were never trapped.
	// Path resolution happens after this reply, which is what makes the lazy
	// bind-shim possible.
	NotifRespContinue = 1

	sizeNotif = 80
	sizeResp  = 24
)

type sockFilter struct {
	code uint16
	jt   uint8
	jf   uint8
	k    uint32
}

type sockFprog struct {
	length uint16
	_      [6]byte
	filter *sockFilter
}

// Notif is one trapped execve.
type Notif struct {
	ID   uint64
	PID  uint32 // translated into the reading process's pid namespace
	NR   int32
	Args [6]uint64
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// checkSizes guards against a kernel whose notification structs differ from
// the layout compiled in here.
func checkSizes() error {
	var buf [6]byte
	if _, _, errno := syscall.Syscall(sysSeccomp, seccompGetNotifSizes, 0,
		uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return fmt.Errorf("seccomp(GET_NOTIF_SIZES): %w", errno)
	}
	notif := binary.LittleEndian.Uint16(buf[0:])
	resp := binary.LittleEndian.Uint16(buf[2:])
	if notif != sizeNotif || resp != sizeResp {
		return fmt.Errorf("kernel seccomp_notif layout is %d/%d, expected %d/%d",
			notif, resp, sizeNotif, sizeResp)
	}
	return nil
}

// InstallExecGate installs the filter on every thread of the caller and returns
// the listener fd. Descendants inherit the filter, so this observes every exec
// in the pod. Requires CAP_SYS_ADMIN in the current user namespace.
func InstallExecGate() (int, error) {
	if err := checkSizes(); err != nil {
		return -1, err
	}
	prog := []sockFilter{
		{0x20, 0, 0, 4},                 // 0: load arch
		{0x15, 1, 0, auditArchX864},     // 1: == x86_64 ? -> 3
		{0x06, 0, 0, retAllow},          // 2: other arch: allow
		{0x20, 0, 0, 0},                 // 3: load syscall nr
		{0x15, 2, 0, 59},                // 4: execve   -> 7
		{0x15, 1, 0, 322},               // 5: execveat -> 7
		{0x06, 0, 0, retAllow},          // 6: allow
		{0x06, 0, 0, retUserNotif},      // 7: notify the supervisor
	}
	fp := sockFprog{length: uint16(len(prog)), filter: &prog[0]}
	fd, _, errno := syscall.Syscall(sysSeccomp, seccompSetModeFilter,
		filterFlagNewListener|filterFlagTSync|filterFlagTSyncESRCH,
		uintptr(unsafe.Pointer(&fp)))
	runtime.KeepAlive(prog)
	if errno != 0 {
		return -1, fmt.Errorf("seccomp(SET_MODE_FILTER): %w", errno)
	}
	return int(fd), nil
}

// NotifRecv blocks until a process in the pod calls execve. The caller must
// reply to every notification it receives or that process stays frozen.
func NotifRecv(fd int) (*Notif, error) {
	buf := make([]byte, sizeNotif)
	if err := ioctl(fd, ioctlNotifRecv, unsafe.Pointer(&buf[0])); err != nil {
		return nil, err
	}
	n := &Notif{
		ID:  binary.LittleEndian.Uint64(buf[0:]),
		PID: binary.LittleEndian.Uint32(buf[8:]),
		NR:  int32(binary.LittleEndian.Uint32(buf[16:])),
	}
	for i := range n.Args {
		n.Args[i] = binary.LittleEndian.Uint64(buf[32+i*8:])
	}
	return n, nil
}

// NotifSend unfreezes a process. errno of 0 with flags NotifRespContinue means
// "carry on with the real syscall".
func NotifSend(fd int, id uint64, val int64, errno int32, flags uint32) error {
	buf := make([]byte, sizeResp)
	binary.LittleEndian.PutUint64(buf[0:], id)
	binary.LittleEndian.PutUint64(buf[8:], uint64(val))
	binary.LittleEndian.PutUint32(buf[16:], uint32(errno))
	binary.LittleEndian.PutUint32(buf[20:], flags)
	return ioctl(fd, ioctlNotifSend, unsafe.Pointer(&buf[0]))
}

// NotifContinue is the common reply.
func NotifContinue(fd int, id uint64) error {
	return NotifSend(fd, id, 0, 0, NotifRespContinue)
}

// NotifIDValid reports whether the notification is still live. Between reading
// a notification and acting on it the process can die and its pid be reused,
// so anything read out of /proc must be validated with this before it is used.
func NotifIDValid(fd int, id uint64) bool {
	return ioctl(fd, ioctlNotifIDValid, unsafe.Pointer(&id)) == nil
}
