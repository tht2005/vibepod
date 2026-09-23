package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"time"

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
	table   *route.Table
	shimAll bool
	podLn   *net.UnixListener

	mu       sync.Mutex
	shims    map[string]string // shadowed path -> stashed original
	sessions map[string]*session
	pins     map[string]string // session id -> pinned target

	rpcMu   sync.Mutex
	nextID  uint64
	pending map[uint64]chan *proto.Msg
}

type session struct {
	id   string
	pid  int
	argv []string
	done chan int
}

func newPodState(d *Daemon, name string, p *pod.Pod, t *route.Table, shimAll bool) *podState {
	return &podState{
		d: d, name: name, p: p, table: t, shimAll: shimAll,
		started:  time.Now(),
		shims:    map[string]string{},
		sessions: map[string]*session{},
		pins:     map[string]string{},
		pending:  map[uint64]chan *proto.Msg{},
	}
}

// readLoop demultiplexes vpinit's replies. Requests carry an id; anything
// without one is an event.
func (s *podState) readLoop() {
	for {
		m, _, err := s.p.Conn.Recv()
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
			ch <- m
		}
	}
}

// call sends a request to vpinit and waits for its reply. Everything it is
// used for is a local syscall away, so a slow reply means something is wrong
// rather than merely busy.
func (s *podState) call(m *proto.Msg, fds ...int) (*proto.Msg, error) {
	s.rpcMu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan *proto.Msg, 1)
	s.pending[id] = ch
	s.rpcMu.Unlock()

	m.ID = id
	if err := s.p.Conn.Send(m, fds...); err != nil {
		s.rpcMu.Lock()
		delete(s.pending, id)
		s.rpcMu.Unlock()
		return nil, err
	}
	select {
	case reply := <-ch:
		if reply.Op == proto.OpErr {
			return nil, fmt.Errorf("%s", reply.Err)
		}
		return reply, nil
	case <-time.After(10 * time.Second):
		s.rpcMu.Lock()
		delete(s.pending, id)
		s.rpcMu.Unlock()
		return nil, fmt.Errorf("vpinit did not answer %q in 10s", m.Op)
	}
}

// ensureShim makes vpsh stand in for path, stashing the original first so the
// shim can still run it. Called while the calling process is frozen in execve,
// so it must finish before the reply or the redirect misses.
func (s *podState) ensureShim(path string) (string, error) {
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

func (s *podState) pinOf(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pins[sessionID]
}

func (s *podState) close() {
	if s.podLn != nil {
		_ = s.podLn.Close()
	}
	s.p.Kill()
}
