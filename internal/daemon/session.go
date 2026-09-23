package daemon

import (
	"fmt"
	"os"
	"sync"

	"vibepod/internal/event"
	"vibepod/internal/pod"
	"vibepod/internal/proto"
	"vibepod/internal/sys"
	"vibepod/internal/term"
)

// scrollback is what a reattaching client sees replayed. Enough to show what
// an agent was doing when you looked away, not enough to be a log.
const scrollback = 256 << 10

// attachment is one terminal watching a session. A session can have none,
// which is exactly what detaching means.
type attachment struct {
	out    *os.File
	stop   chan struct{}
	closed sync.Once
}

func (a *attachment) done() { a.closed.Do(func() { close(a.stop) }) }

// startSession runs a process in the pod on a pty the daemon owns.
//
// The daemon holding the master is the whole of detach and reattach: the
// process writes to a terminal that does not care whether anyone is reading.
func (s *podState) startSession(m *proto.Msg, kind string) (*session, error) {
	id := m.Session
	if id == "" {
		id = s.nextSessionID()
	}
	env := append([]string{}, m.Env...)
	env = append(env,
		"VIBEPOD_POD="+s.name,
		"VIBEPOD_SESSION="+id,
		"VIBEPOD_SOCK="+pod.SockPath,
		"PATH="+podPath(m.Env),
	)
	s.claimSession(id)
	reply, err := s.callFD(&proto.Msg{
		Op: proto.OpSpawn, Argv: m.Argv, Env: env, Cwd: m.Cwd,
		AllocPTY: true, Rows: m.Rows, Cols: m.Cols, Session: id,
	})
	if err != nil {
		return nil, err
	}
	if len(reply.fds) != 1 {
		closeAll(reply.fds)
		return nil, fmt.Errorf("vpinit did not return a terminal")
	}
	sess := &session{
		id: id, kind: kind, argv: m.Argv, pid: reply.msg.Pid,
		master:  os.NewFile(uintptr(reply.fds[0]), "pty"),
		ring:    term.NewRing(scrollback),
		done:    make(chan int, 1),
		clients: map[*attachment]bool{},
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()

	go sess.pump()
	s.d.bus.Publish(event.Event{Kind: event.KindSession, Pod: s.name,
		Session: id, PID: sess.pid, Argv: m.Argv, Detail: "started"})
	return sess, nil
}

// pump moves the session's output into the scrollback and out to whoever is
// attached. It runs whether anyone is watching or not.
func (sess *session) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := sess.master.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = sess.ring.Write(chunk)
			sess.mu.Lock()
			for a := range sess.clients {
				if _, err := a.out.Write(chunk); err != nil {
					a.done()
				}
			}
			sess.mu.Unlock()
		}
		if err != nil {
			sess.mu.Lock()
			for a := range sess.clients {
				a.done()
			}
			sess.mu.Unlock()
			return
		}
	}
}

// attach wires a client's terminal to a session until the session ends or the
// client detaches. Returns the exit code, and whether it merely detached.
//
// Only the *output* half uses the client's descriptor. Input arrives as
// messages, because the daemon must never be the thing reading a client's
// terminal: a read blocked on a tty does not reliably come back when the
// descriptor is closed, and a daemon still holding that read goes on eating
// keystrokes that belong to whoever comes next.
func (sess *session) attach(out *os.File, detachCh <-chan struct{}) (code int, detached bool) {
	a := &attachment{out: out, stop: make(chan struct{})}
	if snap := sess.ring.Snapshot(); len(snap) > 0 {
		_, _ = out.Write(snap)
	}
	sess.mu.Lock()
	sess.clients[a] = true
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		delete(sess.clients, a)
		sess.mu.Unlock()
	}()

	select {
	case code = <-sess.done:
		sess.done <- code // leave it for anyone else waiting
		return code, false
	case <-detachCh:
		return 0, true
	case <-a.stop:
		select {
		case code = <-sess.done:
			sess.done <- code
		default:
		}
		return code, false
	}
}

// write feeds keystrokes to the session's terminal.
func (sess *session) write(data []byte) {
	if sess.master != nil && len(data) > 0 {
		_, _ = sess.master.Write(data)
	}
}

func (sess *session) resize(rows, cols int) {
	if sess.master != nil && rows > 0 && cols > 0 {
		_ = sys.SetWinsize(sess.master.Fd(), rows, cols)
	}
}

func (sess *session) close() {
	if sess.master != nil {
		_ = sess.master.Close()
	}
}

// nextSessionID counts within the pod. Short and stable, so that
// `vpctl attach work 2` is something a person can type from what they saw in
// the tree — and so that two sessions starting at once cannot collide.
func (s *podState) nextSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionSeq++
	return fmt.Sprintf("%d", s.sessionSeq)
}

// findSession locates a running session by id, or the only one if unnamed.
func (s *podState) findSession(id string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" {
		sess, ok := s.sessions[id]
		if !ok {
			return nil, fmt.Errorf("no session %q in pod %s", id, s.name)
		}
		if sess.master == nil {
			return nil, fmt.Errorf("session %s has no terminal to attach to", id)
		}
		return sess, nil
	}
	var found *session
	for _, sess := range s.sessions {
		if sess.master == nil {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("pod %s has several sessions; name one", s.name)
		}
		found = sess
	}
	if found == nil {
		return nil, fmt.Errorf("pod %s has no attachable session", s.name)
	}
	return found, nil
}

// endSession retires a finished session.
func (s *podState) endSession(sess *session) {
	s.mu.Lock()
	delete(s.sessions, sess.id)
	s.mu.Unlock()
	sess.close()
	s.d.bus.Publish(event.Event{Kind: event.KindSession, Pod: s.name,
		Session: sess.id, Detail: "ended"})
}
