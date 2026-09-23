package daemon

import (
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"vibepod/internal/fs"
	"vibepod/internal/pod"
	"vibepod/internal/proto"
	"vibepod/internal/route"
)

// podState is everything the daemon keeps about one running pod.
type podState struct {
	d       *Daemon
	name    string
	started time.Time
	p       *pod.Pod
	podLn   *net.UnixListener
	fs      *fs.Manager
	// configPath is the file this pod was created from, so `vp save` writes
	// back to it rather than guessing.
	configPath string

	// tbl is swapped, never mutated. Three goroutines read it — the pod
	// socket, the tree, and every dispatch — and `vp mount` replaces it while
	// they do, which is the one piece §9 named as frozen.
	tbl atomic.Pointer[route.Table]

	mu       sync.Mutex
	sessions map[string]*session
	// backends is the heart of v2: one machine per session, chosen and never
	// inferred. Absent means the pod's default.
	backends map[string]string
	// toldLocal remembers which sessions have already been told that their
	// shell stays in the pod even though their backend is elsewhere. Said once
	// is information; said every command is noise.
	toldLocal map[string]bool
	// mounts is the live mount list, in the order the mounts were added.
	// Order is part of the state: it decides what shadows what.
	mounts []*mountRec
	// toolHosts maps a name in /vp/bin to the machine the config said has it.
	toolHosts map[string]string
	canMount  []string
	// machines are the backends the config named, whether or not they own a
	// mount.
	machines []string
	// consented is the machines allowed to hold one pushed binary. Nothing is
	// copied anywhere without an entry here.
	consented map[string]bool
	envMode   EnvPolicy
	used      map[string]bool // machines this pod has reached
	// nodePods are the pods this pod has on other machines: the same composed
	// zone, at the same paths, on each backend that runs one.
	nodePods nodePods
	// relays serve this machine's directories to nodes, keyed by where they are in
	// the pod; relayLinks are each node's reverse forward to one of them; reach
	// remembers whether a node can reach an owner by itself.
	relays     map[string]*relayServer
	relayLinks map[string]*relayLink
	relaySeq   int
	reach      map[string]string
	// credentials are the machines allowed this machine's ssh agent, and agents the
	// socket each one has, once forwarded.
	credentials map[string]bool
	agents      map[string]string
	homes       map[string]string
	// ports are the forwards this pod opened, closed when it goes down.
	ports []proto.PortSpec
	// lease is how long a node pod outlives silence from this daemon. It is the only
	// thing that keeps one alive, and the only thing that cleans one up.
	lease string
	// gen is the version of the desired mount list. Every change bumps it, and a
	// node pod that has converged to a lower one is behind — on the mounts it is
	// missing, not on the machine as a whole.
	gen int64
	// writers is which machine may write to each mount, and manyWriters the mounts
	// where the config said not to bother deciding.
	writers     map[string]string
	manyWriters map[string]bool
	// waits is how a pod process's exit code gets back to whoever started it.
	// vpinit reports exits by pid, and a session can hold several processes —
	// one shell per backend it has visited — so the pid is the only key that
	// identifies one.
	waits      map[int]chan int
	sessionSeq int
	mountCount int
	// pendingSession names the session whose first process has been asked for
	// but has not yet started.
	pendingSession string
	execs          map[int]*execRec // the program each live pid is running
	history        []*execRec       // programs that have ended

	stopped chan struct{}

	rpcMu   sync.Mutex
	nextID  uint64
	pending map[uint64]chan rpcReply
}

func newPodState(d *Daemon, name string, p *pod.Pod, t *route.Table) *podState {
	s := &podState{
		d: d, name: name, p: p,
		started:   time.Now(),
		sessions:  map[string]*session{},
		backends:  map[string]string{},
		toldLocal: map[string]bool{},
		waits:     map[int]chan int{},
		execs:     map[int]*execRec{},
		pending:   map[uint64]chan rpcReply{},
		stopped:   make(chan struct{}),
	}
	s.tbl.Store(t)
	return s
}

// table is the current path map. Callers must not hold it across a mount.
func (s *podState) table() *route.Table { return s.tbl.Load() }

// readLoop demultiplexes vpinit's replies. Requests carry an id; anything
// without one is an event.
func (s *podState) readLoop() {
	for {
		m, fds, err := s.p.Conn.Recv()
		if err != nil {
			s.d.logf("pod %s: vpinit link closed: %v", s.name, err)
			s.d.removePod(s.name)
			return
		}
		if m.Op == proto.OpExited {
			s.mu.Lock()
			ch := s.waits[m.Pid]
			delete(s.waits, m.Pid)
			s.mu.Unlock()
			if ch != nil {
				select {
				case ch <- m.Code:
				default:
				}
			}
			continue
		}
		s.rpcMu.Lock()
		ch := s.pending[m.ID]
		delete(s.pending, m.ID)
		s.rpcMu.Unlock()
		if ch != nil {
			ch <- rpcReply{msg: m, fds: fds}
		} else {
			closeAll(fds)
		}
	}
}

// rpcReply pairs vpinit's answer with any descriptor it handed back.
type rpcReply struct {
	msg *proto.Msg
	fds []int
}

// call sends a request to vpinit and waits for its reply. Everything it is used
// for is a local syscall away, so a slow reply means something is wrong rather
// than merely busy.
func (s *podState) call(m *proto.Msg, fds ...int) (*proto.Msg, error) {
	r, err := s.callFD(m, fds...)
	if err != nil {
		return nil, err
	}
	closeAll(r.fds)
	return r.msg, nil
}

func (s *podState) callFD(m *proto.Msg, fds ...int) (rpcReply, error) {
	s.rpcMu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan rpcReply, 1)
	s.pending[id] = ch
	s.rpcMu.Unlock()

	m.ID = id
	if err := s.p.Conn.Send(m, fds...); err != nil {
		s.rpcMu.Lock()
		delete(s.pending, id)
		s.rpcMu.Unlock()
		return rpcReply{}, err
	}
	select {
	case reply := <-ch:
		if reply.msg.Op == proto.OpErr {
			closeAll(reply.fds)
			return rpcReply{}, fmt.Errorf("%s", reply.msg.Err)
		}
		return reply, nil
	case <-time.After(10 * time.Second):
		s.rpcMu.Lock()
		delete(s.pending, id)
		s.rpcMu.Unlock()
		return rpcReply{}, fmt.Errorf("vpinit did not answer %q in 10s", m.Op)
	}
}

// spawnInPod starts a process in the pod and returns its pid and a channel that
// will carry its exit code. Registering the waiter before the spawn is not
// tidiness: vpinit reports the exit, and a short-lived process can finish before
// the reply to the spawn has been read.
func (s *podState) spawnInPod(m *proto.Msg, fds ...int) (int, chan int, *os.File, error) {
	wait := make(chan int, 1)
	s.claimSession(m.Session)
	reply, err := s.callFD(m, fds...)
	if err != nil {
		return 0, nil, nil, err
	}
	var master *os.File
	if len(reply.fds) == 1 {
		master = os.NewFile(uintptr(reply.fds[0]), "pty")
	} else {
		closeAll(reply.fds)
	}
	pid := reply.msg.Pid
	s.mu.Lock()
	s.waits[pid] = wait
	s.mu.Unlock()
	return pid, wait, master, nil
}

// runToCompletion starts a process in the pod and waits for it, which is what
// `vp @pod cmd` and a non-terminal `vp run` both are.
func (s *podState) runToCompletion(m *proto.Msg, fds []int) (int, error) {
	pid, wait, master, err := s.spawnInPod(m, fds...)
	if err != nil {
		return 0, err
	}
	if master != nil {
		master.Close()
	}
	select {
	case code := <-wait:
		return code, nil
	case <-s.stopped:
		s.mu.Lock()
		delete(s.waits, pid)
		s.mu.Unlock()
		return 0, fmt.Errorf("pod %s went down while %v was running", s.name, m.Argv)
	}
}

// claimSession labels a session's first process, which is identified by having
// vpinit as its parent.
func (s *podState) claimSession(id string) {
	s.mu.Lock()
	s.pendingSession = id
	s.mu.Unlock()
}

func (s *podState) sessionForRoot(ppid int) string {
	if ppid != s.p.Pid {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.pendingSession
	s.pendingSession = ""
	return id
}

// baselineEnv is the environment a session started with, against which a
// dispatched command's environment is a delta.
func (s *podState) baselineEnv(sessionID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[sessionID]; sess != nil {
		return sess.env
	}
	return nil
}

func (s *podState) close() {
	select {
	case <-s.stopped:
	default:
		close(s.stopped)
	}
	if s.podLn != nil {
		_ = s.podLn.Close()
	}
	// Live shells on other machines first: each is an ssh, and killing the pod
	// does not reach them.
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.close()
	}
	// Forwards ride the multiplexed connections, so they go before anything tears
	// those down.
	s.closePorts()
	// Node pods next: each is a namespace on another machine holding mounts of its
	// own, and killing this pod does not reach them.
	for _, np := range s.nodePods.list() {
		s.stopNodePod(np.host)
	}
	s.stopRelays()
	// Then the pod: its processes hold the mounts open.
	s.p.Kill()
	if s.fs != nil {
		s.fs.Unmount()
	}
	for _, h := range s.hostsUsed() {
		_ = s.d.pool.Host(h).Cleanup()
	}
}

// hostsUsed lists the machines this pod ever reached, so its remote state can
// be cleaned up when it goes down.
func (s *podState) hostsUsed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.used))
	for h := range s.used {
		out = append(out, h)
	}
	return out
}

func (s *podState) markUsed(host string) {
	s.mu.Lock()
	if s.used == nil {
		s.used = map[string]bool{}
	}
	s.used[host] = true
	s.mu.Unlock()
}
