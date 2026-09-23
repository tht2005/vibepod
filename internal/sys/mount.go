package sys

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	MsBind    = syscall.MS_BIND
	MsRec     = syscall.MS_REC
	MsPrivate = syscall.MS_PRIVATE
	MsRdonly  = syscall.MS_RDONLY
	MsRemount = syscall.MS_REMOUNT
	MsNosuid  = syscall.MS_NOSUID
	MsNodev   = syscall.MS_NODEV
)

func Mount(src, dst, fstype string, flags uintptr, data string) error {
	if err := syscall.Mount(src, dst, fstype, flags, data); err != nil {
		return fmt.Errorf("mount %s -> %s (%s): %w", src, dst, fstype, err)
	}
	return nil
}

// BindOver binds src onto dst, creating dst as a file or directory to match.
// Every mount a pod has is one of these, whether it was made at `up` or an hour
// later — vpinit keeps a thread with CAP_SYS_ADMIN for exactly that reason.
func BindOver(src, dst string, readonly bool) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			f.Close()
		}
	}
	if err := Mount(src, dst, "", MsBind|MsRec, ""); err != nil {
		return err
	}
	if readonly {
		return RemountReadOnly(dst)
	}
	return nil
}

// Unbind detaches a mount lazily, so a dead link cannot wedge the pod. Lazily
// is the only honest option here: a process may still have a file open under it,
// and the alternative is refusing to unmount a directory nobody can reach.
func Unbind(dst string) error {
	if err := syscall.Unmount(dst, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount %s: %w", dst, err)
	}
	return nil
}

func PivotRoot(newRoot string) error {
	if err := os.Chdir(newRoot); err != nil {
		return err
	}
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	// The old root is now stacked on "/" — detach it so the pod cannot reach
	// anything that was not explicitly bound in.
	if err := syscall.Unmount("/", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	return os.Chdir("/")
}
