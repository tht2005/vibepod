package pod

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
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

	if err := conn.Send(&proto.Msg{Op: proto.OpHello, Pid: os.Getpid()}); err != nil {
		return fmt.Errorf("announce the pod: %w", err)
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
	remove   bool
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
// mount in the pod happens here, for the pod's whole life — which is what makes
// `vp mount` into a running pod possible at all: a machine added an hour in is
// the same bind as one named in the config.
func (s *initServer) mountWorker(ready chan<- error) {
	runtime.LockOSThread()
	ready <- nil
	for req := range s.binds {
		if req.remove {
			req.reply <- sys.Unbind(req.dst)
			continue
		}
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
// A serve loop that waited for the work it dispatched would be waiting on a
// message it is itself responsible for reading. That cost a real deadlock once,
// when a spawn's execve had to be answered by this same loop; the gate that
// caused it is gone, but a bind requested while a spawn is in flight has the
// same shape, so the rule stays.
func (s *initServer) serve() error {
	for {
		m, fds, err := s.conn.Recv()
		if err != nil {
			// The daemon is gone; so is the pod.
			return nil
		}
		switch m.Op {
		case proto.OpBind, proto.OpUnbind:
			go s.handleBind(m, fds)
			fds = nil
		case proto.OpSpawn:
			go s.handleSpawn(m, fds)
		case proto.OpSignal:
			closeAll(fds)
			_ = syscall.Kill(m.Pid, syscall.Signal(m.Sig))
			s.send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
		default:
			closeAll(fds)
			s.send(&proto.Msg{Op: proto.OpErr, ID: m.ID,
				Err: fmt.Sprintf("unknown op %q", m.Op)})
		}
	}
}

// handleBind performs a mount asked for after the pod exists, which is all
// `vp mount` needs from vpinit. The source is a path in the staging area, where
// the daemon's mount arrived by propagation — see StageDir for why it cannot
// simply be the host path.
func (s *initServer) handleBind(m *proto.Msg, fds []int) {
	defer closeAll(fds)
	reply := make(chan error, 1)
	s.binds <- &bindReq{src: m.Src, dst: m.Dst, readonly: m.ReadOnly,
		remove: m.Op == proto.OpUnbind, reply: reply}
	if err := <-reply; err != nil {
		s.send(&proto.Msg{Op: proto.OpErr, ID: m.ID, Err: err.Error()})
		return
	}
	// The staged copy has served its purpose. Leaving it would give those files a
	// second path in the pod, and a path under /vp means nothing on the machine
	// that owns them.
	if m.Op == proto.OpBind && strings.HasPrefix(m.Src, StageDir+"/") {
		s.binds <- &bindReq{dst: m.Src, remove: true, reply: reply}
		if err := <-reply; err != nil {
			s.send(&proto.Msg{Op: proto.OpOK, ID: m.ID,
				Detail: "the staged copy at " + m.Src + " could not be released: " +
					err.Error()})
			return
		}
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
