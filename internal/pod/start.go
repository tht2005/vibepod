package pod

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"vibepod/internal/proto"
	"vibepod/internal/sys"
)

// Pod is the daemon's handle on a running pod.
type Pod struct {
	Spec *proto.Spec
	Pid  int // host pid of vpinit
	Conn *proto.Conn
	cmd  *exec.Cmd
}

// Start clones vpinit into fresh namespaces and waits for it to say the pod
// exists.
//
// The capability hand-off is the subtle part: the child holds a full set in the
// new user namespace, but execve would drop it for a non-root euid, so
// CAP_SYS_ADMIN is raised into the *ambient* set, which survives exec. vpinit
// clears ambient immediately afterwards, so the capability stops there.
func Start(spec *proto.Spec) (*Pod, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	pair, err := syscall.Socketpair(syscall.AF_UNIX,
		syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	ours, theirs := pair[0], pair[1]
	childEnd := os.NewFile(uintptr(theirs), "vpinit")
	defer childEnd.Close()

	cmd := exec.Command(self, "vpinit")
	cmd.ExtraFiles = []*os.File{childEnd} // becomes fd 3 in vpinit
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS |
			syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
		AmbientCaps:                []uintptr{sys.CapSysAdmin, sys.CapDacOverride},
		Pdeathsig:                  syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		syscall.Close(ours)
		return nil, fmt.Errorf("start vpinit: %w", err)
	}
	conn, err := proto.FromFD(ours, "vpinit")
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	if err := conn.Send(&proto.Msg{Op: proto.OpInit, Spec: spec}); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("send spec: %w", err)
	}
	m, fds, err := conn.Recv()
	closeAll(fds)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("await pod: %w", err)
	}
	if m.Op == proto.OpErr {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("vpinit: %s", m.Err)
	}
	if m.Op != proto.OpHello {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("expected hello from vpinit, got %q", m.Op)
	}
	return &Pod{Spec: spec, Pid: cmd.Process.Pid, Conn: conn, cmd: cmd}, nil
}

func (p *Pod) Kill() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	}
	if p.Conn != nil {
		_ = p.Conn.Close()
	}
}

// Wait blocks until the pod's PID 1 exits, which tears down the namespace and
// everything in it.
func (p *Pod) Wait() error { return p.cmd.Wait() }
