package daemon

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/remote"
	"vibepod/internal/route"
	"vibepod/internal/sys"
)

// Backends: which machine a session's commands run on.
//
// v1 guessed this from the working directory, through a seccomp gate on execve.
// v2 chooses it. The whole of that change is here and in session.go: a map from
// session to machine, a live shell per pair, and one place that dispatches.

// backendOf is the machine this session is on. Per-session, never global: the
// human in the TUI and the agent in the pod are separate sessions with separate
// backends, so neither can move the other's ground.
func (s *podState) backendOf(session string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.backends[session]; ok && b != "" {
		return b
	}
	return s.table().Default
}

// knownBackend reports whether a name is a machine this pod can actually reach,
// so `vp use gpu5` fails at the moment of the typo rather than at the next
// command.
func (s *podState) knownBackend(name string) bool {
	if name == route.Pod {
		return true
	}
	for _, r := range s.table().Rules() {
		if r.Owner == name || r.ExecOn == name {
			return true
		}
	}
	// A machine with no mount of its own is still a backend: gpu05 has the GPUs
	// and none of the data, which is the case §6 is about.
	for _, m := range s.declaredMachines() {
		if m == name {
			return true
		}
	}
	return false
}

func (s *podState) declaredMachines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.machines...)
}

// setBackend moves a session. If that session has a terminal, the shell its
// keystrokes reach is swapped too; the one it left keeps its cwd and its jobs.
func (s *podState) setBackend(session, backend string) error {
	if session == "" {
		return fmt.Errorf("a backend belongs to a session, and none was named")
	}
	if backend == "" || backend == "auto" {
		// v1 spelled "let the directory decide" as auto. Nothing decides any
		// more, so the honest reading is the pod's default.
		backend = s.table().Default
	}
	if !s.knownBackend(backend) {
		return fmt.Errorf("this pod has no machine called %q; `vp hosts` lists them", backend)
	}
	s.mu.Lock()
	sess := s.sessions[session]
	s.backends[session] = backend
	delete(s.toldLocal, session)
	s.mu.Unlock()

	s.d.bus.Publish(event.Event{Kind: event.KindSession, Pod: s.name,
		Session: session, Target: backend, Detail: "backend " + backend})
	s.d.logf("pod %s: session %s is now on %s", s.name, session, backend)
	if sess == nil || !sess.interactive() {
		return nil
	}
	return sess.switchTo(s, backend)
}

// resolveTool answers the question a /vp/bin wrapper cannot: which machine
// should run this name?
//
// The session's backend comes first, because a session that has moved to gpu05
// means it. Otherwise the config said, or exactly one machine is mounted and
// there is nothing to choose between. Anything else is a refusal that names the
// candidates, because picking one would be a guess about where work goes.
func (s *podState) resolveTool(session, tool string) (string, error) {
	if b := s.backendOf(session); b != route.Pod {
		return b, nil
	}
	s.mu.Lock()
	host := s.toolHosts[tool]
	s.mu.Unlock()
	if host != "" {
		return host, nil
	}
	machines := s.reachable()
	switch len(machines) {
	case 0:
		return "", fmt.Errorf("%s is not on this machine, and this pod has no "+
			"others; mount one with `vp mount <host>:<path>`", tool)
	case 1:
		return machines[0], nil
	}
	return "", fmt.Errorf("%s could run on %s; this session is in the pod, so say "+
		"which: `vp @<machine> %s …`, or `vp use <machine>` first",
		tool, strings.Join(machines, " or "), tool)
}

// reachable lists the remote machines this pod knows about.
func (s *podState) reachable() []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range s.declaredMachines() {
		if name != route.Pod && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, r := range s.table().Rules() {
		for _, name := range []string{r.Owner, r.ExecOn} {
			if name == "" || name == route.Pod || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// dispatchReq is one command going somewhere, with the caller's own
// descriptors: the daemon decides where and then gets out of the data path.
type dispatchReq struct {
	session string
	backend string
	cwd     string
	argv    []string
	env     []string
	tty     bool
	files   []*os.File
	// pid is the caller's pid as the daemon's namespace sees it, when the
	// caller is inside the pod. It parents the record in the tree.
	pid int
	// onStart hands back the running host and exec id, so a Ctrl-C arriving on
	// the same connection can be forwarded while the command runs.
	onStart func(*remote.Host, string)
}

// dispatch runs one command on one machine and returns its exit status.
func (s *podState) dispatch(r dispatchReq) (int, error) {
	if len(r.argv) == 0 {
		return 0, fmt.Errorf("nothing to run")
	}
	if len(r.files) < 3 {
		return 0, fmt.Errorf("a dispatched command needs stdin, stdout and stderr")
	}
	backend := r.backend
	if backend == "" {
		backend = s.backendOf(r.session)
	}
	if backend != route.Pod && !s.knownBackend(backend) {
		return 0, fmt.Errorf("this pod has no machine called %q; `vp hosts` lists them",
			backend)
	}
	// The pod's own machinery never leaves this machine. A command in /vp is
	// the socket, the wrappers or the brief, and shipping any of those to a
	// remote would be a bug wearing a routing decision's clothes.
	if backend != route.Pod && route.Private(r.cwd) {
		return 0, fmt.Errorf("%s is the pod's own directory and does not exist on %s",
			r.cwd, backend)
	}
	if backend == route.Pod {
		return s.dispatchLocal(r)
	}

	dir := route.Dir(s.table(), r.cwd, backend)
	// Which pod, if any, this command runs in on the far side.
	//
	// If the directory belongs to the machine we are sending it to, it is that
	// machine's own path and a plain ssh is both correct and the fastest thing
	// available. If it belongs to somebody else — or to no mount at all — then
	// the same string over there means something different, or nothing, and the
	// only safe answer is the pod that reproduces it. DESIGN.md §6: the bad
	// outcome is not the path missing, it is the path existing and holding other
	// bytes.
	owner := route.Owner(s.table(), r.cwd)
	np := s.nodePods.get(backend)
	if np == nil && owner != backend {
		// Consent given earlier means "and keep doing that": build the pod now
		// rather than making the caller ask for something they already allowed.
		if err := s.ensureNodePod(backend); err == nil {
			np = s.nodePods.get(backend)
		}
	}
	switch {
	case np != nil && owner == route.Pod:
		// One of this machine's own directories, which no node pod holds until
		// reverse mounts exist. Same answer as with no pod at all: run in that
		// machine's home, and say so once.
		dir = ""
		s.noteHomeDir(r.session, backend, r.cwd)
	case np != nil:
		// The composed zone is over there at the same paths, so the directory is
		// carried as it is. If this particular mount is missing from that pod, say
		// so — a stale node costs a visible refusal, never a wrong path.
		if !np.holds(dir) && owner != backend {
			// Behind on this mount. Try once to catch up — it may simply have been
			// unreachable when the mount was added — and refuse only if that fails.
			if err := s.reconcile(np); err != nil || !np.holds(dir) {
				return 0, fmt.Errorf("%s is not in the pod on %s yet, so a command "+
					"there would be in a directory of the same name on a different "+
					"filesystem; `vp node` shows what it is behind on", dir, backend)
			}
		}
	case owner == backend:
		// The directory is that machine's own: the path means there what it says,
		// and a plain ssh is both correct and the fastest thing available.
	case owner != route.Pod:
		// It belongs to a *third* machine. This is the case §6 is about: the same
		// string over there is either missing or holds other bytes, and the second
		// outcome succeeds, writes somewhere real, and is found days later.
		return 0, s.needNodePod(backend, r.cwd)
	default:
		// A directory of this machine's own, or none at all. The command is
		// something like `rocm-smi` that does not care where it runs, so it runs in
		// that machine's own home rather than being refused — and the fact that the
		// directory did not travel is said once, not assumed.
		dir = ""
		s.noteHomeDir(r.session, backend, r.cwd)
	}

	rec := &execRec{PID: r.pid, PPID: parentOf(r.pid), Argv: r.argv, Cwd: r.cwd,
		Target: backend, Session: r.session, Start: now()}
	s.recordExec(rec)

	host := s.d.pool.Host(backend)
	if np != nil {
		host = host.InPod(np.home, s.nodeName(backend))
	}
	s.markUsed(backend)
	id := fmt.Sprintf("%s-%d", s.name, execSeq.Add(1))
	s.mu.Lock()
	rec.ExecID = id
	s.mu.Unlock()
	if r.onStart != nil {
		r.onStart(host, id)
	}

	// Carry what the caller set for this command, and nothing else. See env.go:
	// the shell stays local, the delta crosses, identity never does.
	env, refused := forwardEnv(s.envMode, s.baselineEnv(r.session), r.env)
	if len(refused) > 0 {
		// A refusal nobody mentions is how the bug this guards against got
		// here. It does not go to the command's stderr — agents parse that —
		// so it goes where the routing decisions go.
		s.d.bus.Publish(event.Event{Kind: event.KindNotice, Pod: s.name,
			Target: backend, Argv: r.argv, Session: r.session,
			Detail: "not forwarded: " + strings.Join(refused, ", ")})
		s.d.logf("pod %s: %v on %s: not forwarding %s (they describe this machine)",
			s.name, r.argv, backend, strings.Join(refused, ", "))
	}
	req := remote.Req{Dir: dir, Argv: r.argv, Env: env, TTY: r.tty, ID: id}
	copy(req.Files[:], r.files[:3])
	code, err := host.Run(req)
	if r.onStart != nil {
		r.onStart(nil, "")
	}

	if err != nil {
		s.finishExec(rec, remote.ExitLinkDown)
		return 0, err
	}
	s.finishExec(rec, code)
	// The command has just finished, and nothing else touches that machine's
	// tree through us — so this is the exact moment the cache may be stale, and
	// the only moment it can have become so.
	if s.fs != nil {
		s.fs.InvalidateAfter(backend)
	}
	// And on every *other* machine holding a cache of the directory it ran in.
	// With one cache, forgetting after a command was enough; with a pod on every
	// backend there are N, and a node that is not told keeps serving the bytes from
	// before the command ran.
	s.fanOutInvalidate(backend, r.cwd)
	return code, nil
}

// dispatchLocal is `vp @pod cmd`, and the wrapper case where the tool turns out
// to live here after all. It runs in the pod on the caller's own descriptors.
func (s *podState) dispatchLocal(r dispatchReq) (int, error) {
	rec := &execRec{PID: r.pid, PPID: parentOf(r.pid), Argv: r.argv, Cwd: r.cwd,
		Target: route.Pod, Session: r.session, Start: now()}
	s.recordExec(rec)

	fds := []int{int(r.files[0].Fd()), int(r.files[1].Fd()), int(r.files[2].Fd())}
	code, err := s.runToCompletion(&proto.Msg{
		Op: proto.OpSpawn, Argv: r.argv, Env: r.env, Cwd: r.cwd,
		TTY: r.tty, Session: r.session,
	}, fds)
	if err != nil {
		s.finishExec(rec, remote.ExitLinkDown)
		return 0, err
	}
	s.finishExec(rec, code)
	return code, nil
}

// noteHomeDir says, once per session and machine, that a dispatched command ran in
// the target's home rather than in the directory it was called from. Once is
// information; every command would be noise, and saying nothing at all is how a
// surprise becomes a bug report.
func (s *podState) noteHomeDir(session, backend, cwd string) {
	key := session + "\x00" + backend
	s.mu.Lock()
	told := s.toldLocal[key]
	s.toldLocal[key] = true
	s.mu.Unlock()
	if told {
		return
	}
	s.d.bus.Publish(event.Event{Kind: event.KindNotice, Pod: s.name, Target: backend,
		Session: session, Detail: fmt.Sprintf("%s is not on %s, so commands there "+
			"start in its own home; `vp node add %s` reproduces the tree instead",
			cwd, backend, backend)})
}

// now is a seam for nothing in particular; it keeps the record construction
// above readable.
func now() time.Time { return time.Now() }

// parentOf is the pid's parent as this namespace sees it, which is what parents
// a command under the shell that ran it in the tree.
func parentOf(pid int) int {
	if pid <= 0 {
		return 0
	}
	return sys.PPID(uint32(pid))
}

// use moves a session's backend, own-session-only on a pod socket.
//
// §9 allows an agent to move itself — that is the interface — but not to move
// somebody else's session. So which session is asking is not the caller's to
// claim: the kernel says which process this is, and its environment says which
// session. From the host socket any session can be named, because the human at
// the terminal is the one whose sessions these are.
func (k *ctlConn) use(m *proto.Msg) error {
	if !k.host {
		id := sessionOf(uint32(k.c.PeerPID()))
		if id == "" {
			return fmt.Errorf("`vp use` moves the session it is run from, and this " +
				"process is not in one")
		}
		if m.Session != "" && m.Session != id {
			return fmt.Errorf("from inside the pod, `vp use` moves this session "+
				"(%s), not %s", id, m.Session)
		}
		m.Session = id
	}
	return k.d.use(m)
}

// use moves a session's backend.
func (d *Daemon) use(m *proto.Msg) error {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return err
	}
	target := m.Backend
	if target == "" {
		target = m.Target
	}
	return s.setBackend(m.Session, target)
}
