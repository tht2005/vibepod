package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"time"

	"os"

	"vibepod/internal/fs"
	"vibepod/internal/pod"
	"vibepod/internal/proto"
	"vibepod/internal/route"
	"vibepod/internal/term"
)

// podState is everything the daemon keeps about one running pod.
type podState struct {
	d       *Daemon
	name    string
	started time.Time
	p       *pod.Pod
	table   *route.Table
	shimAll bool
	podLn   *net.UnixListener
	fs      *fs.Manager

	shimOnce sync.Mutex

	mu       sync.Mutex
	shims    map[string]string // shadowed path -> stashed original
	sessions map[string]*session
	pins     map[string]string // session id -> pinned target
	// envMode is how much of a caller's environment crosses to a remote.
	envMode    EnvPolicy
	used       map[string]bool // hosts this pod has routed to
	sessionSeq int
	// pendingSession names the session whose first process has been asked for
	// but has not yet reached execve.
	pendingSession string
	execs          map[int]*execRec // the program each live pid is running
	history        []*execRec       // programs that have ended

	stopped chan struct{}

	rpcMu   sync.Mutex
	nextID  uint64
	pending map[uint64]chan rpcReply
}

type session struct {
	id   string
	kind string // "console", "shell", "agent"
	pid  int
	argv []string
	// env is the baseline this session started with. A routed command carries
	// the difference between its own environment and this, which is exactly
	// what the caller set and nothing else.
	env  []string
	done chan int

	master *os.File
	ring   *term.Ring

	mu      sync.Mutex
	clients map[*attachment]bool
}

func newPodState(d *Daemon, name string, p *pod.Pod, t *route.Table, shimAll bool) *podState {
	return &podState{
		d: d, name: name, p: p, table: t, shimAll: shimAll,
		started:  time.Now(),
		shims:    map[string]string{},
		sessions: map[string]*session{},
		pins:     map[string]string{},
		execs:    map[int]*execRec{},
		pending:  map[uint64]chan rpcReply{},
		stopped:  make(chan struct{}),
	}
}

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
			sess := s.sessions[m.Session]
			s.mu.Unlock()
			if sess != nil {
				select {
				case sess.done <- m.Code:
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

// call sends a request to vpinit and waits for its reply. Everything it is
// used for is a local syscall away, so a slow reply means something is wrong
// rather than merely busy.
// rpcReply pairs vpinit's answer with any descriptor it handed back.
type rpcReply struct {
	msg *proto.Msg
	fds []int
}

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

// ensureShim makes vpsh stand in for path, stashing the original first so the
// shim can still run it. Called while the calling process is frozen in execve,
// so it must finish before the reply or the redirect misses.
func (s *podState) ensureShim(path string) (string, error) {
	// One binding per binary, even when several processes reach it at once:
	// a second bind would stack a shim on top of a shim.
	s.shimOnce.Lock()
	defer s.shimOnce.Unlock()

	s.mu.Lock()
	if stash, ok := s.shims[path]; ok {
		s.mu.Unlock()
		return stash, nil
	}
	s.mu.Unlock()

	sum := sha256.Sum256([]byte(path))
	stash := filepath.Join(pod.StashDir,
		hex.EncodeToString(sum[:8])+"-"+filepath.Base(path))

	// Order matters: once vpsh is bound over path, the original is
	// unreachable by name.
	if _, err := s.call(&proto.Msg{Op: proto.OpBind, Src: path, Dst: stash}); err != nil {
		return "", fmt.Errorf("stash %s: %w", path, err)
	}
	if _, err := s.call(&proto.Msg{Op: proto.OpBind, Src: pod.ShimPath, Dst: path}); err != nil {
		return "", fmt.Errorf("shim %s: %w", path, err)
	}
	s.mu.Lock()
	s.shims[path] = stash
	s.mu.Unlock()
	s.d.logf("pod %s: shimmed %s (stash %s)", s.name, path, stash)
	return stash, nil
}

// stashOf reports where the original of a shadowed binary now lives.
func (s *podState) stashOf(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stash, ok := s.shims[path]
	return stash, ok
}

// claimSession labels a session's first process.
//
// The exec gate reads a process's environment before execve replaces it, so
// the very first exec of a session still carries vpinit's environment and not
// the session token. Everything descended from it inherits the token
// normally; only the root needs this. It is identified by its parent, which
// is vpinit itself.
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
// command's own environment is a delta.
func (s *podState) baselineEnv(sessionID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[sessionID]; sess != nil {
		return sess.env
	}
	return nil
}

func (s *podState) pinOf(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pins[sessionID]
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
	// Kill the pod first: its processes hold the mounts open.
	s.p.Kill()
	if s.fs != nil {
		s.fs.Unmount()
	}
	for _, h := range s.hostsUsed() {
		_ = s.d.pool.Host(h).Cleanup()
	}
}

// hostsUsed lists the machines this pod ever routed to, so its remote state
// can be cleaned up when it goes down.
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
