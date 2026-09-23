package pod

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"vibepod/internal/proto"
	"vibepod/internal/sys"
)

// Paths inside a pod. /vp is the pod's own machinery: the socket back to the
// daemon, the shim binary, and the stash of the binaries the shim shadows.
const (
	VpDir    = "/vp"
	RunDir   = "/vp/run"
	SockPath = "/vp/run/pod.sock"
	ShimPath = "/vp/bin/vpsh"
	CtlPath  = "/vp/bin/vpctl"
	BinDir   = "/vp/bin"
	StashDir = "/vp/real"
)

// systemBinds are mounted read-only into every pod. They are the host's own
// toolchain, which is the point: nothing is installed to run an agent here.
var systemBinds = []string{"/usr", "/etc", "/opt"}

var rootSymlinks = [][2]string{
	{"usr/bin", "bin"}, {"usr/sbin", "sbin"},
	{"usr/lib", "lib"}, {"usr/lib64", "lib64"},
}

var devNodes = []string{"null", "zero", "full", "random", "urandom", "tty"}

// buildRoot assembles the pod filesystem and pivots into it. It runs as PID 1
// of a fresh mount namespace, so nothing here is visible to the host.
func buildRoot(spec *proto.Spec) error {
	// Detach from host propagation first: everything below is ours alone.
	if err := sys.Mount("", "/", "", sys.MsRec|sys.MsPrivate, ""); err != nil {
		return err
	}
	root := spec.Root
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if err := sys.Mount("tmpfs", root, "tmpfs", 0, "mode=755"); err != nil {
		return err
	}
	in := func(p string) string { return filepath.Join(root, p) }

	for _, d := range systemBinds {
		if _, err := os.Stat(d); err != nil {
			continue
		}
		if err := sys.BindOver(d, in(d), true); err != nil {
			return err
		}
	}
	for _, l := range rootSymlinks {
		if err := os.Symlink(l[0], in(l[1])); err != nil && !os.IsExist(err) {
			return err
		}
	}

	if err := os.MkdirAll(in("/proc"), 0o755); err != nil {
		return err
	}
	if err := sys.Mount("proc", in("/proc"), "proc",
		sys.MsNosuid|sys.MsNodev|syscall.MS_NOEXEC, ""); err != nil {
		return err
	}
	if err := buildDev(in("/dev")); err != nil {
		return err
	}
	if err := os.MkdirAll(in("/tmp"), 0o755); err != nil {
		return err
	}
	if err := sys.Mount("tmpfs", in("/tmp"), "tmpfs", sys.MsNosuid|sys.MsNodev,
		"mode=1777"); err != nil {
		return err
	}
	if err := buildVp(in(VpDir), spec); err != nil {
		return err
	}

	// User mounts last: a later mount may legitimately sit on top of /usr etc.
	// only if the caller asked for it, and vpctl up has already refused the
	// dangerous ones.
	for _, b := range spec.Binds {
		if err := sys.BindOver(b.Src, in(b.Dst), b.ReadOnly); err != nil {
			return fmt.Errorf("bind %s: %w", b.Dst, err)
		}
	}
	return sys.PivotRoot(root)
}

func buildDev(dev string) error {
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return err
	}
	if err := sys.Mount("tmpfs", dev, "tmpfs", sys.MsNosuid, "mode=755"); err != nil {
		return err
	}
	for _, n := range devNodes {
		if err := sys.BindOver("/dev/"+n, filepath.Join(dev, n), false); err != nil {
			return err
		}
	}
	pts := filepath.Join(dev, "pts")
	if err := os.MkdirAll(pts, 0o755); err != nil {
		return err
	}
	// A private devpts instance: pod terminals are not host terminals.
	if err := sys.Mount("devpts", pts, "devpts", sys.MsNosuid|syscall.MS_NOEXEC,
		"newinstance,ptmxmode=0666,mode=0620"); err != nil {
		return err
	}
	if err := os.Symlink("pts/ptmx", filepath.Join(dev, "ptmx")); err != nil &&
		!os.IsExist(err) {
		return err
	}
	shm := filepath.Join(dev, "shm")
	if err := os.MkdirAll(shm, 0o755); err != nil {
		return err
	}
	return sys.Mount("tmpfs", shm, "tmpfs", sys.MsNosuid|sys.MsNodev, "mode=1777")
}

// buildVp lays out the pod's machinery and then makes the directory
// unwritable, so the agent cannot plant anything where a shim will land.
func buildVp(vp string, spec *proto.Spec) error {
	if err := os.MkdirAll(vp, 0o755); err != nil {
		return err
	}
	if err := sys.Mount("tmpfs", vp, "tmpfs", sys.MsNosuid, "mode=755"); err != nil {
		return err
	}
	for _, d := range []string{"run", "bin", "real"} {
		if err := os.MkdirAll(filepath.Join(vp, d), 0o755); err != nil {
			return err
		}
	}
	if err := sys.BindOver(spec.RunDir, filepath.Join(vp, "run"), false); err != nil {
		return err
	}
	if err := sys.BindOver(spec.ShimBin, filepath.Join(vp, "bin", "vpsh"), true); err != nil {
		return err
	}
	// The agent's own view of vibepod: read-only, and limited by which socket
	// it can reach rather than by what the binary can do.
	if spec.CtlBin != "" {
		if err := sys.BindOver(spec.CtlBin, filepath.Join(vp, "bin", "vpctl"), true); err != nil {
			return err
		}
	}
	return os.Chmod(vp, 0o555)
}
