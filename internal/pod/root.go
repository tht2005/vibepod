package pod

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"vibepod/internal/proto"
	"vibepod/internal/sys"
)

// Paths inside a pod. /vp is the pod's own machinery: the socket back to the
// daemon, the two binaries, and the wrappers that lead PATH.
const (
	VpDir     = "/vp"
	RunDir    = "/vp/run"
	SockPath  = "/vp/run/pod.sock"
	ShellPath = "/vp/bin/vpsh"
	VpPath    = "/vp/bin/vp"
	BinDir    = "/vp/bin"
	// StageDir is where a mount made after `up` arrives.
	//
	// vpinit pivoted into the pod's root long ago, so the host directory a FUSE
	// mount lives at cannot be named from in here — and the kernel refuses a
	// bind whose source mount belongs to another mount namespace, however that
	// source is named. What does cross is propagation: this is a bind of the
	// daemon's mount directory, kept as a slave of it, so a mount the daemon
	// makes appears here and can then be bound where it belongs. vpinit unmounts
	// the staged copy immediately afterwards, so a remote directory still has
	// exactly one path in the pod.
	StageDir = "/vp/mnt"
	// BriefPath is the generated agent instruction file. It lives under
	// /vp/run, which is a bind of the pod's own runtime directory on the host,
	// so the daemon can rewrite it when a mount is added without asking vpinit
	// for anything.
	BriefPath = "/vp/run/brief.md"
)

// StagedPath is where a host mountpoint appears inside a pod, once the daemon
// has made it and propagation has carried it into the staging area.
func StagedPath(hostMountPoint string) string {
	return filepath.Join(StageDir, filepath.Base(hostMountPoint))
}

// briefLinks are where an agent will actually look. All three read a project
// instruction file from the working directory upwards, so a copy at the root of
// the pod's filesystem is found without anyone editing a repository — which is
// the point: the pod must not write into the user's checkout.
var briefLinks = []string{"/CLAUDE.md", "/AGENTS.md"}

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
	// Receive mounts from the namespace we were cloned from, and send none back.
	//
	// v1 detached entirely (MS_PRIVATE), which was simpler and made `vp mount`
	// impossible: a pod could only ever have the mounts it was born with. Slave
	// is one-way — the pod learns about a mount the daemon makes, and can never
	// place one outside itself. What it can reach that way is exactly the staging
	// directory below; everything else the host mounts lives under the old root,
	// which pivot_root detaches.
	if err := sys.Mount("", "/", "", sys.MsRec|sys.MsSlave, ""); err != nil {
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
	if err := bindResolvConf(root); err != nil {
		return err
	}
	if err := buildVp(in(VpDir), spec); err != nil {
		return err
	}
	if err := writeBrief(root, spec); err != nil {
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

// bindResolvConf makes name resolution work inside the pod.
//
// A pod shares the host's network namespace, so it can already reach
// everything the host can — but on a systemd-resolved machine
// /etc/resolv.conf is a symlink into /run, which is not bound, and the
// symlink dangles. The symptom is an agent that starts perfectly and then
// times out on its first API call.
func bindResolvConf(root string) error {
	target, err := filepath.EvalSymlinks("/etc/resolv.conf")
	if err != nil || target == "/etc/resolv.conf" {
		return nil // a real file inside /etc, already bound
	}
	if _, err := os.Stat(target); err != nil {
		return nil
	}
	return sys.BindOver(target, filepath.Join(root, target), true)
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
// unwritable, so nothing in the pod can replace a wrapper that leads PATH.
func buildVp(vp string, spec *proto.Spec) error {
	if err := os.MkdirAll(vp, 0o755); err != nil {
		return err
	}
	if err := sys.Mount("tmpfs", vp, "tmpfs", sys.MsNosuid, "mode=755"); err != nil {
		return err
	}
	for _, d := range []string{"run", "bin", "mnt"} {
		if err := os.MkdirAll(filepath.Join(vp, d), 0o755); err != nil {
			return err
		}
	}
	if err := sys.BindOver(spec.RunDir, filepath.Join(vp, "run"), false); err != nil {
		return err
	}
	// The staging area. Non-recursive, so the mounts already made for this pod
	// are not copied in — they are bound at their own paths below, and a second
	// path to the same files is the one thing the pod's layout must not have.
	if spec.StageDir != "" {
		stage := filepath.Join(vp, "mnt")
		if err := sys.BindOne(spec.StageDir, stage, false); err != nil {
			return fmt.Errorf("stage %s: %w", spec.StageDir, err)
		}
		if err := sys.MakeSlave(stage); err != nil {
			return err
		}
	}
	// The pod's $SHELL, which records a command line and then runs it. A node pod
	// has neither: there is no agent on a node to record, and the recording
	// belongs to the machine that dispatched the command.
	if spec.ShellBin != "" {
		if err := sys.BindOver(spec.ShellBin, filepath.Join(vp, "bin", "vpsh"),
			true); err != nil {
			return err
		}
	}
	// The agent's own view of vibepod: read-only, and limited by which socket
	// it can reach rather than by what the binary can do.
	if spec.CtlBin != "" {
		if err := sys.BindOver(spec.CtlBin, filepath.Join(vp, "bin", "vp"), true); err != nil {
			return err
		}
	}
	// A wrapper per tool the agent should not run here: one that exists only on
	// a remote, or one that exists in both places and belongs on the other.
	// /vp/bin leads PATH, so a bare `rocm-smi` finds this; an absolute path to
	// a binary this machine does not have still fails, as it must.
	for _, name := range spec.Tools {
		if name == "" || strings.ContainsRune(name, '/') {
			continue
		}
		if err := writeWrapper(filepath.Join(vp, "bin", name), name); err != nil {
			return fmt.Errorf("wrapper for %s: %w", name, err)
		}
	}
	return os.Chmod(vp, 0o555)
}

// writeWrapper is the whole of what replaced the bind-shims: three lines that
// hand the command to `vp`, which asks the daemon which machine this session is
// on. No kernel help, nothing to unmount, and it is what `remote_tools:` always
// meant.
func writeWrapper(path, name string) error {
	script := fmt.Sprintf("#!/bin/sh\n"+
		"# vibepod: %s runs on this session's backend.\n"+
		"exec %s tool %s \"$@\"\n", name, VpPath, name)
	return os.WriteFile(path, []byte(script), 0o555)
}

// writeBrief puts the generated instruction file where an agent will find it,
// and links it from the root of the pod's filesystem.
//
// With no exec gate this is not a nicety: an agent that has not been told about
// `vp` will run everything in the pod, over FUSE, on the wrong machine.
func writeBrief(root string, spec *proto.Spec) error {
	if spec.Brief == "" {
		return nil
	}
	if err := os.WriteFile(filepath.Join(spec.RunDir, "brief.md"),
		[]byte(spec.Brief), 0o644); err != nil {
		return err
	}
	for _, link := range briefLinks {
		p := filepath.Join(root, link)
		if _, err := os.Lstat(p); err == nil {
			continue
		}
		if err := os.Symlink(BriefPath, p); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}
