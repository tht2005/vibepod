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
	sendMu sync.Mutex

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
	pid    int
	master *os.File // pty master, when the session asked for a terminal
	err    error
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
		pid, master, err := s.doSpawn(req.m, req.fds)
		req.reply <- spawnRes{pid: pid, master: master, err: err}
	}
}

// serve dispatches, and never waits.
//
// This matters more than it looks. Starting a process means forking and
// waiting for its execve to complete — and that execve traps to the daemon,
// which may need to ask vpinit to bind a shim before it will let it through.
// A serve loop that waited for the spawn it was performing would be waiting
// on a message it is itself responsible for reading.
func (s *initServer) serve() error {
	for {
		m, fds, err := s.conn.Recv()
		if err != nil {
			// The daemon is gone; so is the pod.
			return nil
		}
		switch m.Op {
		case proto.OpBind:
			go s.handleBind(m)
		case proto.OpSpawn:
			go s.handleSpawn(m, fds)
		case proto.OpSignal:
			_ = syscall.Kill(m.Pid, syscall.Signal(m.Sig))
			s.send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
		default:
			s.send(&proto.Msg{Op: proto.OpErr, ID: m.ID,
				Err: fmt.Sprintf("unknown op %q", m.Op)})
		}
	}
}

func (s *initServer) handleBind(m *proto.Msg) {
	reply := make(chan error, 1)
	s.binds <- &bindReq{src: m.Src, dst: m.Dst, readonly: m.ReadOnly, reply: reply}
	if err := <-reply; err != nil {
		s.send(&proto.Msg{Op: proto.OpErr, ID: m.ID, Err: err.Error()})
		return
	}
	s.send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
}

func (s *initServer) handleSpawn(m *proto.Msg, fds []int) {
	defer closeAll(fds)
	reply := make(chan spawnRes, 1)
	s.spawns <- &spawnReq{m: m, fds: fds, reply: reply}
	res := <-reply
	switch {
	case res.err != nil:
		s.send(&proto.Msg{Op: proto.OpErr, ID: m.ID, Err: res.err.Error()})
	case res.master != nil:
		// The daemon keeps the master, which is what lets the session outlive
		// the terminal that started it.
		s.send(&proto.Msg{Op: proto.OpSpawned, ID: m.ID, Pid: res.pid,
			Session: m.Session}, int(res.master.Fd()))
		res.master.Close()
	default:
		s.send(&proto.Msg{Op: proto.OpSpawned, ID: m.ID, Pid: res.pid,
			Session: m.Session})
	}
}

// send serialises replies from the workers onto the single socket.
func (s *initServer) send(m *proto.Msg, fds ...int) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	_ = s.conn.Send(m, fds...)
}

// doSpawn starts a process on the caller's own file descriptors. Passing fds
// rather than copying bytes keeps vpinit out of the data path entirely.
// It runs only on the capability-free thread; see spawnWorker.
func (s *initServer) doSpawn(m *proto.Msg, fds []int) (int, *os.File, error) {
	if len(m.Argv) == 0 {
		return 0, nil, fmt.Errorf("spawn needs argv")
	}
	var master, slave *os.File
	var files []uintptr
	if m.AllocPTY {
		var err error
		// From the pod's own devpts instance, not the host's.
		master, slave, err = sys.OpenPTY()
		if err != nil {
			return 0, nil, err
		}
		defer slave.Close()
		if m.Rows > 0 && m.Cols > 0 {
			_ = sys.SetWinsize(master.Fd(), m.Rows, m.Cols)
		}
		files = []uintptr{slave.Fd(), slave.Fd(), slave.Fd()}
	} else {
		if len(fds) < 3 {
			return 0, nil, fmt.Errorf("spawn needs 3 fds, got %d", len(fds))
		}
		files = []uintptr{uintptr(fds[0]), uintptr(fds[1]), uintptr(fds[2])}
	}
	attr := &syscall.ProcAttr{
		Dir:   m.Cwd,
		Env:   m.Env,
		Files: files,
		Sys:   &syscall.SysProcAttr{Setsid: true},
	}
	if m.TTY || m.AllocPTY {
		attr.Sys.Setctty = true
		attr.Sys.Ctty = 0
	}
	path, err := lookPath(m.Argv[0], m.Env)
	if err != nil {
		if master != nil {
			master.Close()
		}
		return 0, nil, err
	}
	pid, err := syscall.ForkExec(path, m.Argv, attr)
	if err != nil {
		if master != nil {
			master.Close()
		}
		return 0, nil, err
	}
	s.mu.Lock()
	s.sessions[pid] = m.Session
	s.mu.Unlock()
	return pid, master, nil
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
