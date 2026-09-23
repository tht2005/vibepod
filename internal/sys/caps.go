package sys

import (
	"fmt"
	"syscall"
	"unsafe"
)

// The kernel's capability sets, version 3 (two 32-bit words per set).
type capHeader struct {
	version uint32
	pid     int32
}

type capData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

const (
	prCapAmbient           = 47
	prCapAmbientClearAll   = 4
	prSetNoNewPrivs        = 38
	CapSysAdmin          = 21
	capVersion3          = 0x20080522
	sysCapset            = 126
	CapDacOverride       = 1
)

// ClearAmbient drops the ambient capability set without touching the permitted
// or effective sets. vpinit calls this once: it keeps CAP_SYS_ADMIN for itself
// (lazy bind-shims need it for the pod's whole life) while guaranteeing that
// every process it spawns, the agent included, starts with nothing.
func ClearAmbient() error {
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prCapAmbient,
		prCapAmbientClearAll, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_CAP_AMBIENT_CLEAR_ALL): %w", errno)
	}
	return nil
}

// DropAllCaps empties every capability set of the *calling thread*.
//
// Linux credentials are per-thread, and a fork inherits the forking thread's.
// vpinit therefore runs its spawner on a dedicated thread that has called
// this, so that nothing it starts can hold a capability, while keeping
// CAP_SYS_ADMIN on the thread that mounts.
func DropAllCaps() error {
	if err := ClearAmbient(); err != nil {
		return err
	}
	hdr := capHeader{version: capVersion3}
	var data [2]capData
	if _, _, errno := syscall.RawSyscall(sysCapset,
		uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("capset(empty): %w", errno)
	}
	return nil
}

// SetNoNewPrivs makes setuid binaries inert for this process and its children.
func SetNoNewPrivs() error {
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs,
		1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", errno)
	}
	return nil
}
