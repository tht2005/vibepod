package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/route"
	"vibepod/internal/sys"
	"vibepod/internal/term"
)

// scrollback is what a reattaching client sees replayed. Enough to show what an
// agent was doing when you looked away, not enough to be a log.
const scrollback = 256 << 10

// A session is a terminal and the shells behind it.
//
// One session, several shells: one in the pod, and one on each machine the
// session has visited. Switching backend swaps which shell the keystrokes reach
// and leaves the other exactly as it was, with its cwd and its jobs — which is
// the whole reason a shell is held open instead of one ssh per command.
type session struct {
	id   string
	kind string // "console", "shell", "run", "tui"
	argv []string
	// env is the baseline this session started with. A dispatched command
	// carries the difference between its own environment and this, which is
	// exactly what the caller set and nothing else.
	env     []string
	started time.Time
	// cwd is where the session began, which is the directory a shell on a new
	// backend opens in. A shell's own cwd after that belongs to the shell.
	cwd string

	mu      sync.Mutex
	shells  map[string]*shellHandle
	cur     *shellHandle
	primary *shellHandle
	rows    int
	cols    int
	clients map[*attachment]bool
	done    chan int
	ended   bool
}

// shellHandle is one shell, in the pod or on another machine. Both kinds look
// the same from here: a pty master, a pid, and a ring buffer.
type shellHandle struct {
	backend string
	master  *os.File
	pid     int
	ring    *term.Ring
	wait    chan int
	cmd     *exec.Cmd // set for a shell on another machine; the ssh
	once    sync.Once
}

// attachment is one terminal watching a session. A session can have none, which
// is exactly what detaching means.
type attachment struct {
	out    *os.File
	stop   chan struct{}
	closed sync.Once
}

func (a *attachment) done() { a.closed.Do(func() { close(a.stop) }) }

func (sess *session) interactive() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.primary != nil && sess.primary.master != nil
}

func (sess *session) backendOfShell() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.cur == nil {
		return ""
	}
	return sess.cur.backend
}

// startSession runs a session's first shell and returns once it exists.
//
// If the session's backend is another machine, that first shell is a shell on
// that machine, and this is where "the session is the shell" becomes literal.
func (s *podState) startSession(m *proto.Msg, kind string) (*session, error) {
	id := m.Session
	if id == "" {
		id = s.nextSessionID()
	}
	backend := s.backendOf(id)
	if m.Backend != "" {
		backend = m.Backend
		s.mu.Lock()
		s.backends[id] = backend
		s.mu.Unlock()
	}
	env := s.sessionEnv(id, backend, m.Env)
	sess := &session{
		id: id, kind: kind, argv: m.Argv, env: env, cwd: m.Cwd,
		started: time.Now(), rows: m.Rows, cols: m.Cols,
		shells:  map[string]*shellHandle{},
		clients: map[*attachment]bool{},
		done:    make(chan int, 1),
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()

	sh, err := s.openShell(sess, backend, m.Argv)
	if err != nil {
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		return nil, err
	}
	sess.mu.Lock()
	sess.primary, sess.cur = sh, sh
	sess.mu.Unlock()

	s.d.bus.Publish(event.Event{Kind: event.KindSession, Pod: s.name,
		Session: id, PID: sh.pid, Argv: m.Argv, Target: backend,
		Detail: "started"})
	return sess, nil
}

// openShell starts one shell for a session, in the pod or on a machine.
//
// argv is honoured only in the pod: a session started to run `claude` runs
// claude, and the same session moved to gpu03 gets gpu03's login shell, because
// the agent is the thing that must not be installed there.
func (s *podState) openShell(sess *session, backend string, argv []string) (*shellHandle, error) {
	sh := &shellHandle{backend: backend, ring: term.NewRing(scrollback),
		wait: make(chan int, 1)}
	sess.mu.Lock()
	rows, cols := sess.rows, sess.cols
	cwd := sess.cwd
	sess.mu.Unlock()

	if backend == route.Pod {
		pid, wait, master, err := s.spawnInPod(&proto.Msg{
			Op: proto.OpSpawn, Argv: argv, Env: sess.env, Cwd: cwd,
			AllocPTY: true, Rows: rows, Cols: cols, Session: sess.id,
		})
		if err != nil {
			return nil, err
		}
		if master == nil {
			return nil, fmt.Errorf("vpinit did not return a terminal")
		}
		sh.pid, sh.wait, sh.master = pid, wait, master
	} else {
		// A pty on this side, so the ring buffer, detach and resize machinery
		// does not know or care that this shell is on another machine.
		master, slave, err := sys.OpenPTY()
		if err != nil {
			return nil, err
		}
		defer slave.Close()
		if rows > 0 && cols > 0 {
			_ = sys.SetWinsize(master.Fd(), rows, cols)
		}
		dir := route.Dir(s.table(), cwd, backend)
		host := s.d.pool.Host(backend)
		cmd, err := host.Shell(dir, slave)
		if err != nil {
			master.Close()
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			master.Close()
			return nil, fmt.Errorf("open a shell on %s: %w", backend, err)
		}
		s.markUsed(backend)
		sh.master, sh.cmd, sh.pid = master, cmd, cmd.Process.Pid
		go func() {
			err := cmd.Wait()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				code = 1
			}
			sh.finish(code)
		}()
	}

	sess.mu.Lock()
	sess.shells[backend] = sh
	sess.mu.Unlock()
	go sess.pump(s, sh)
	return sh, nil
}

// switchTo moves an interactive session's keystrokes to another machine's shell,
// opening it if this is the first visit. The shell left behind keeps running.
func (sess *session) switchTo(s *podState, backend string) error {
	sess.mu.Lock()
	if sess.cur != nil && sess.cur.backend == backend {
		sess.mu.Unlock()
		return nil
	}
	sh := sess.shells[backend]
	sess.mu.Unlock()

	if sh == nil {
		var err error
		if sh, err = s.openShell(sess, backend, sess.argv); err != nil {
			return err
		}
	}
	sess.mu.Lock()
	sess.cur = sh
	clients := make([]*attachment, 0, len(sess.clients))
	for a := range sess.clients {
		clients = append(clients, a)
	}
	snap := sh.ring.Snapshot()
	sess.mu.Unlock()

	// Redraw from this shell's own scrollback, so the screen shows the machine
	// you just moved to rather than a mix of two.
	banner := fmt.Sprintf("\r\n\x1b[2m[vibepod: this terminal is now on %s]\x1b[0m\r\n",
		backend)
	// Nothing is installed on that machine — that is the premise — so `vp` is not
	// there, and the way back has to be said rather than discovered.
	if backend != route.Pod {
		banner += fmt.Sprintf("\x1b[2m[vibepod: vp lives in the pod, not there; "+
			"`vp use pod -s %s` from another terminal, or b in the cockpit, "+
			"moves it back]\x1b[0m\r\n", sess.id)
	}
	for _, a := range clients {
		if _, err := a.out.Write([]byte(banner)); err != nil {
			a.done()
			continue
		}
		if len(snap) > 0 {
			_, _ = a.out.Write(snap)
		}
	}
	return nil
}

// pump moves one shell's output into its own scrollback, and out to whoever is
// attached — but only while that shell is the one the session is showing. A
// shell on a machine you switched away from goes on working into its ring.
func (sess *session) pump(s *podState, sh *shellHandle) {
	buf := make([]byte, 32<<10)
	for {
		n, err := sh.master.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = sh.ring.Write(chunk)
			sess.mu.Lock()
			if sess.cur == sh {
				for a := range sess.clients {
					if _, err := a.out.Write(chunk); err != nil {
						a.done()
					}
				}
			}
			sess.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	// The primary shell ending ends the session; any other shell just goes
	// away, and the session falls back to the one it started with.
	sess.mu.Lock()
	primary := sess.primary == sh
	fallback := route.Pod
	if !primary {
		delete(sess.shells, sh.backend)
		if sess.primary != nil {
			fallback = sess.primary.backend
		}
		if sess.cur == sh {
			sess.cur = sess.primary
		}
	}
	clients := make([]*attachment, 0, len(sess.clients))
	for a := range sess.clients {
		clients = append(clients, a)
	}
	sess.mu.Unlock()

	if primary {
		code := 0
		select {
		case code = <-sh.wait:
		case <-time.After(2 * time.Second):
		}
		sess.mu.Lock()
		sess.ended = true
		sess.mu.Unlock()
		select {
		case sess.done <- code:
		default:
		}
		for _, a := range clients {
			a.done()
		}
		return
	}
	if s != nil {
		s.mu.Lock()
		s.backends[sess.id] = fallback
		s.mu.Unlock()
		note := fmt.Sprintf("\r\n\x1b[2m[vibepod: the shell on %s ended; back on %s]\x1b[0m\r\n",
			sh.backend, fallback)
		for _, a := range clients {
			_, _ = a.out.Write([]byte(note))
		}
	}
	sh.master.Close()
}

// finish records a shell's exit status once, whichever half notices first.
func (sh *shellHandle) finish(code int) {
	sh.once.Do(func() {
		select {
		case sh.wait <- code:
		default:
		}
	})
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
	sess.mu.Lock()
	var snap []byte
	if sess.cur != nil {
		snap = sess.cur.ring.Snapshot()
	}
	sess.clients[a] = true
	sess.mu.Unlock()
	if len(snap) > 0 {
		_, _ = out.Write(snap)
	}
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

// write feeds keystrokes to whichever shell this session is showing.
func (sess *session) write(data []byte) {
	sess.mu.Lock()
	cur := sess.cur
	sess.mu.Unlock()
	if cur != nil && cur.master != nil && len(data) > 0 {
		_, _ = cur.master.Write(data)
	}
}

// resize applies to every shell, not only the visible one: a shell you come
// back to should not be the size the window used to be.
func (sess *session) resize(rows, cols int) {
	if rows <= 0 || cols <= 0 {
		return
	}
	sess.mu.Lock()
	sess.rows, sess.cols = rows, cols
	shells := make([]*shellHandle, 0, len(sess.shells))
	for _, sh := range sess.shells {
		shells = append(shells, sh)
	}
	sess.mu.Unlock()
	for _, sh := range shells {
		if sh.master != nil {
			_ = sys.SetWinsize(sh.master.Fd(), rows, cols)
		}
	}
}

func (sess *session) close() {
	sess.mu.Lock()
	shells := make([]*shellHandle, 0, len(sess.shells))
	for _, sh := range sess.shells {
		shells = append(shells, sh)
	}
	sess.shells = map[string]*shellHandle{}
	sess.cur, sess.primary = nil, nil
	sess.mu.Unlock()
	for _, sh := range shells {
		// An ssh is the daemon's own child and outlives the pod otherwise:
		// killing the pod does not reach a shell on another machine.
		if sh.cmd != nil && sh.cmd.Process != nil {
			_ = sh.cmd.Process.Kill()
		}
		if sh.master != nil {
			_ = sh.master.Close()
		}
	}
}

// nextSessionID counts within the pod. Short and stable, so that
// `vp attach work 2` is something a person can type from what they saw in the
// tree — and so that two sessions starting at once cannot collide.
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
		if !sess.interactive() {
			return nil, fmt.Errorf("session %s has no terminal to attach to", id)
		}
		return sess, nil
	}
	var found *session
	for _, sess := range s.sessions {
		if !sess.interactive() {
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
	delete(s.backends, sess.id)
	delete(s.toldLocal, sess.id)
	s.mu.Unlock()
	sess.close()
	s.d.bus.Publish(event.Event{Kind: event.KindSession, Pod: s.name,
		Session: sess.id, Detail: "ended"})
}

// live reports the sessions and the machine each is on, which is what `vp ps`
// and the TUI both need and nothing else can answer.
func (s *podState) live() []proto.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proto.SessionInfo, 0, len(s.sessions))
	for id, sess := range s.sessions {
		backend := s.backends[id]
		if backend == "" {
			backend = s.table().Default
		}
		if shown := sess.backendOfShell(); shown != "" {
			backend = shown
		}
		out = append(out, proto.SessionInfo{
			ID: id, Kind: sess.kind, Backend: backend, Argv: sess.argv,
			Uptime: time.Since(sess.started).Truncate(time.Second).String(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
