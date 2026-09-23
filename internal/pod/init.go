package pod

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"

	"vibepod/internal/proto"
	"vibepod/internal/sys"
)

// initConnFD is the socketpair the daemon hands vpinit at clone time. Using an
// inherited fd rather than a path means the link exists before the pod does.
const initConnFD = 3

// RunInit is vpinit: PID 1 of a pod.
//
// It exists because a mount namespace only survives while a process is inside
// it, and it is the pod's only holder of CAP_SYS_ADMIN — every process it
// spawns has an empty capability set, so the agent can neither mount nor
// unmount, and cannot remove a shim.
func RunInit() error {
	conn, err := proto.FromFD(initConnFD, "daemon")
	if err != nil {
		return fmt.Errorf("adopt daemon socket: %w", err)
	}
	defer conn.Close()

	m, _, err := conn.Recv()
	if err != nil {
		return fmt.Errorf("await spec: %w", err)
	}
	if m.Op != proto.OpInit || m.Spec == nil {
		return fmt.Errorf("expected %q, got %q", proto.OpInit, m.Op)
	}
	spec := m.Spec

	if err := buildRoot(spec); err != nil {
		return fmt.Errorf("build root: %w", err)
	}
	if spec.Hostname != "" {
		_ = syscall.Sethostname([]byte(spec.Hostname))
	}
	// Setuid binaries are already inert with one uid mapped; this makes it
	// explicit, and keeps fusermount3 unusable inside the pod.
	if err := sys.SetNoNewPrivs(); err != nil {
		return err
	}

	gateFD, err := sys.InstallExecGate()
	if err != nil {
		return fmt.Errorf("install exec gate: %w", err)
	}
	if err := conn.Send(&proto.Msg{Op: proto.OpHello, Pid: os.Getpid()}, gateFD); err != nil {
		return fmt.Errorf("export listener fd: %w", err)
	}
	syscall.Close(gateFD)
	if m, _, err := conn.Recv(); err != nil {
		return fmt.Errorf("await gate ack: %w", err)
	} else if m.Op != proto.OpOK {
		return fmt.Errorf("gate not accepted: %s", m.Err)
	}


	in := &initServer{
		conn:     conn,
		sessions: map[int]string{},
		binds:    make(chan *bindReq),
		spawns:   make(chan *spawnReq),
	}
	ready := make(chan error, 2)
	go in.mountWorker(ready)
	go in.spawnWorker(ready)
	for i := 0; i < 2; i++ {
		if err := <-ready; err != nil {
			return err
		}
	}
	go in.reap()
	return in.serve()
}

type initServer struct {
	conn *proto.Conn

	binds  chan *bindReq
	spawns chan *spawnReq

	mu       sync.Mutex
	sessions map[int]string // pid -> session id
}

type bindReq struct {
	src, dst string
	readonly bool
	reply    chan error
}

type spawnReq struct {
	m     *proto.Msg
	fds   []int
	reply chan spawnRes
}

type spawnRes struct {
	pid int
	err error
}

// mountWorker is pinned to the one thread that keeps CAP_SYS_ADMIN. Every
// mount in the pod happens here, for the pod's whole life: lazy shims arrive
// long after startup.
func (s *initServer) mountWorker(ready chan<- error) {
	runtime.LockOSThread()
	ready <- nil
	for req := range s.binds {
		req.reply <- sys.BindOver(req.src, req.dst, req.readonly)
	}
}

// spawnWorker is pinned to a thread with no capabilities at all, so that
// everything forked from it — the agent, its shells, everything they start —
// begins with an empty set and cannot touch the pod's mounts.
func (s *initServer) spawnWorker(ready chan<- error) {
	runtime.LockOSThread()
	if err := sys.DropAllCaps(); err != nil {
		ready <- err
		return
	}
	ready <- nil
	for req := range s.spawns {
		pid, err := s.doSpawn(req.m, req.fds)
		req.reply <- spawnRes{pid: pid, err: err}
	}
}

func (s *initServer) serve() error {
	for {
		m, fds, err := s.conn.Recv()
		if err != nil {
			// The daemon is gone; so is the pod.
			return nil
		}
		switch m.Op {
		case proto.OpBind:
			reply := make(chan error, 1)
			s.binds <- &bindReq{src: m.Src, dst: m.Dst, readonly: m.ReadOnly, reply: reply}
			err := <-reply
			if err != nil {
				_ = s.conn.Errorf(m.ID, "%v", err)
			} else {
				_ = s.conn.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
			}
		case proto.OpSpawn:
			reply := make(chan spawnRes, 1)
			s.spawns <- &spawnReq{m: m, fds: fds, reply: reply}
			res := <-reply
			pid, err := res.pid, res.err
			closeAll(fds)
			if err != nil {
				_ = s.conn.Errorf(m.ID, "%v", err)
			} else {
				_ = s.conn.Send(&proto.Msg{Op: proto.OpSpawned, ID: m.ID,
					Pid: pid, Session: m.Session})
			}
		case proto.OpSignal:
			_ = syscall.Kill(m.Pid, syscall.Signal(m.Sig))
			_ = s.conn.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
		default:
			_ = s.conn.Errorf(m.ID, "unknown op %q", m.Op)
		}
	}
}

// doSpawn starts a process on the caller's own file descriptors. Passing fds
// rather than copying bytes keeps vpinit out of the data path entirely.
// It runs only on the capability-free thread; see spawnWorker.
func (s *initServer) doSpawn(m *proto.Msg, fds []int) (int, error) {
	if len(fds) < 3 {
		return 0, fmt.Errorf("spawn needs 3 fds, got %d", len(fds))
	}
	if len(m.Argv) == 0 {
		return 0, fmt.Errorf("spawn needs argv")
	}
	attr := &syscall.ProcAttr{
		Dir:   m.Cwd,
		Env:   m.Env,
		Files: []uintptr{uintptr(fds[0]), uintptr(fds[1]), uintptr(fds[2])},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	}
	if m.TTY {
		attr.Sys.Setctty = true
		attr.Sys.Ctty = 0
	}
	path, err := lookPath(m.Argv[0], m.Env)
	if err != nil {
		return 0, err
	}
	pid, err := syscall.ForkExec(path, m.Argv, attr)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.sessions[pid] = m.Session
	s.mu.Unlock()
	return pid, nil
}

// reap collects children. As PID 1 the pod's orphans land here, so this is not
// only about our own sessions.
func (s *initServer) reap() {
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, syscall.SIGCHLD)
	for range ch {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			s.mu.Lock()
			session, tracked := s.sessions[pid]
			delete(s.sessions, pid)
			s.mu.Unlock()
			if tracked {
				_ = s.conn.Send(&proto.Msg{Op: proto.OpExited, Session: session,
					Pid: pid, Code: exitCode(ws)})
			}
		}
	}
}

func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

func closeAll(fds []int) {
	for _, fd := range fds {
		syscall.Close(fd)
	}
}
