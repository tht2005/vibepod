// Package daemon is the vibepod user daemon: one per user, rootless,
// auto-spawned. It owns everything long-lived — the pod registry, the route
// tables, the ssh connections, the live shells and the audit log.
//
// Nothing here runs as root, and no private key ever enters a pod.
package daemon

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"vibepod/internal/config"
	"vibepod/internal/event"
	"vibepod/internal/pod"
	"vibepod/internal/proto"
	"vibepod/internal/remote"
	"vibepod/internal/route"
)

type Daemon struct {
	runDir string
	logger *log.Logger
	pool   *remote.Pool
	bus    *event.Bus

	mu   sync.Mutex
	pods map[string]*podState
	// creating holds, per name, a channel closed when that pod's creation
	// finishes. Creating a pod takes long enough — namespaces, and possibly a
	// remote mount — that two clients racing on the same name would otherwise
	// both pass the existence check and one whole pod would be orphaned.
	creating map[string]chan struct{}
}

// Version is stamped into the binary; the daemon reports it so a client can
// tell whether the daemon it is talking to is the one it was built with.
var Version = "dev"

// RunDir is where the daemon keeps its sockets and per-pod state.
func RunDir() string {
	if x := os.Getenv("VIBEPOD_RUNDIR"); x != "" {
		return x
	}
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), fmt.Sprintf("vibepod-%d", os.Getuid()))
	}
	return filepath.Join(base, "vibepod")
}

func HostSock() string { return filepath.Join(RunDir(), "host.sock") }

func New(runDir string, logger *log.Logger) (*Daemon, error) {
	pool, err := remote.NewPool(filepath.Join(runDir, "ssh"))
	if err != nil {
		return nil, err
	}
	return &Daemon{runDir: runDir, logger: logger, pool: pool,
		bus: event.NewBus(4000), pods: map[string]*podState{},
		creating: map[string]chan struct{}{}}, nil
}

func (d *Daemon) logf(format string, a ...any) {
	if d.logger != nil {
		d.logger.Printf(format, a...)
	}
}

// Serve accepts control connections until the listener closes.
func (d *Daemon) Serve(ln *net.UnixListener) error {
	defer d.shutdown()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			return err
		}
		go d.handle(proto.NewConn(c), true)
	}
}

func (d *Daemon) shutdown() {
	d.mu.Lock()
	pods := make([]*podState, 0, len(d.pods))
	for _, s := range d.pods {
		pods = append(pods, s)
	}
	d.pods = map[string]*podState{}
	d.mu.Unlock()
	for _, s := range pods {
		s.close()
	}
	d.pool.Close()
}

// ctlConn is one client connection. It keeps reading while a session is
// attached, because a window resize or a detach arrives on this same socket and
// must not queue behind the session it is about.
type ctlConn struct {
	d    *Daemon
	c    *proto.Conn
	host bool // false for connections that arrived on a pod socket
	// pod is set for a connection that arrived on a pod socket, which already
	// knows which pod it belongs to and must not be told otherwise.
	pod *podState

	mu     sync.Mutex
	sess   *session
	detach chan struct{}
	// remoteHost and remoteExec name the command this connection is currently
	// running elsewhere, so a Ctrl-C arriving here can be forwarded to it.
	remoteHost *remote.Host
	remoteExec string
}

// handle serves one client. host is false for connections arriving on a pod
// socket, which is what enforces the capability split in DESIGN.md §9.
func (d *Daemon) handle(c *proto.Conn, host bool) {
	(&ctlConn{d: d, c: c, host: host}).serve()
}

func (k *ctlConn) serve() {
	defer k.c.Close()
	for {
		m, fds, err := k.c.Recv()
		if err != nil {
			closeAll(fds)
			return
		}
		if !k.host && !podOp(m.Op) {
			closeAll(fds)
			_ = k.c.Errorf(m.ID, "%q is not permitted from inside a pod", m.Op)
			continue
		}
		d := k.d
		switch m.Op {
		case proto.OpUp:
			d.reply(k.c, m, d.up(m, k.progress(m.ID)))
		case proto.OpPs:
			_ = k.c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Pods: d.ps(),
				Version: Version})
		case proto.OpDown:
			d.reply(k.c, m, d.down(m.Pod))
		case proto.OpLog:
			d.streamLog(k.c, m)
		case proto.OpHosts:
			hosts, err := d.hostList(m)
			if err != nil {
				_ = k.c.Errorf(m.ID, "%v", err)
			} else {
				_ = k.c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Hosts: hosts})
			}
		case proto.OpStat:
			reply, err := d.statInPod(m)
			if err != nil {
				_ = k.c.Errorf(m.ID, "%v", err)
			} else {
				reply.ID = m.ID
				_ = k.c.Send(reply)
			}
		case proto.OpTree:
			t, err := d.treeOf(m)
			if err != nil {
				_ = k.c.Errorf(m.ID, "%v", err)
			} else {
				_ = k.c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Tree: t})
			}
		case proto.OpBackend:
			s, err := d.lookup(m.Pod)
			if err != nil {
				_ = k.c.Errorf(m.ID, "%v", err)
			} else {
				_ = k.c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID,
					Backend: s.backendOf(m.Session), Target: s.table().Default})
			}
		case proto.OpUse:
			d.reply(k.c, m, k.use(m))
		case proto.OpBrief:
			s, err := d.lookup(m.Pod)
			if err != nil {
				_ = k.c.Errorf(m.ID, "%v", err)
			} else {
				_ = k.c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Detail: s.brief()})
			}
		case proto.OpMount:
			d.reply(k.c, m, d.mount(m, k.progress(m.ID), k.host))
		case proto.OpUnmount:
			d.reply(k.c, m, d.unmount(m))
		case proto.OpSave:
			d.saveReply(k.c, m)
		case proto.OpDispatch:
			files := adopt(fds)
			go func() {
				defer closeFiles(files)
				k.runDispatch(m, files)
			}()
		case proto.OpSession, proto.OpAttach:
			go k.runSession(m, fds)
		case proto.OpWinch:
			if sess := k.attached(); sess != nil {
				sess.resize(m.Rows, m.Cols)
			}
		case proto.OpInput:
			if sess := k.attached(); sess != nil {
				sess.write(m.Data)
			}
		case proto.OpExec:
			k.recordShellCommand(m)
			_ = k.c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
		case proto.OpSignal:
			k.forwardSignal(m.Sig)
		case proto.OpDetach:
			k.mu.Lock()
			if k.detach != nil {
				close(k.detach)
				k.detach = nil
			}
			k.mu.Unlock()
		default:
			closeAll(fds)
			_ = k.c.Errorf(m.ID, "unknown op %q", m.Op)
		}
	}
}

// runSession starts or rejoins a terminal session and stays with it until it
// ends or the user detaches.
func (k *ctlConn) runSession(m *proto.Msg, fds []int) {
	files := adopt(fds)
	defer closeFiles(files)
	if len(files) < 3 {
		_ = k.c.Errorf(m.ID, "a session needs stdin, stdout and stderr")
		return
	}
	s, err := k.d.lookup(m.Pod)
	if err != nil {
		_ = k.c.Errorf(m.ID, "%v", err)
		return
	}
	// Without a terminal there is nothing to detach from, so the process runs
	// directly on the caller's own descriptors and the daemon stays out of the
	// data path entirely.
	if m.Op == proto.OpSession && !m.TTY {
		code, err := k.d.runDirect(s, m, files)
		if err != nil {
			_ = k.c.Errorf(m.ID, "%v", err)
			return
		}
		_ = k.c.Send(&proto.Msg{Op: proto.OpExit, ID: m.ID, Code: code})
		return
	}

	var sess *session
	if m.Op == proto.OpAttach {
		sess, err = s.findSession(m.Session)
	} else {
		sess, err = s.startSession(m, sessionKind(m))
	}
	if err != nil {
		_ = k.c.Errorf(m.ID, "%v", err)
		return
	}
	detach := make(chan struct{})
	k.mu.Lock()
	k.sess, k.detach = sess, detach
	k.mu.Unlock()

	code, detached := sess.attach(files[1], detach)
	k.mu.Lock()
	k.sess, k.detach = nil, nil
	k.mu.Unlock()
	if detached {
		_ = k.c.Send(&proto.Msg{Op: proto.OpDetach, ID: m.ID, Session: sess.id})
		return
	}
	s.endSession(sess)
	_ = k.c.Send(&proto.Msg{Op: proto.OpExit, ID: m.ID, Code: code,
		Session: sess.id})
}

// runDispatch is `vp @host cmd` and every /vp/bin wrapper: one command, on one
// machine, on the caller's own descriptors.
func (k *ctlConn) runDispatch(m *proto.Msg, files []*os.File) {
	s, err := k.d.lookup(m.Pod)
	if err != nil {
		_ = k.c.Errorf(m.ID, "%v", err)
		return
	}
	session := m.Session
	pid := 0
	if !k.host {
		// Inside the pod the session is not the caller's to claim: the kernel
		// says which process this is, and its environment says which session.
		pid = k.c.PeerPID()
		if id := sessionOf(uint32(pid)); id != "" {
			session = id
		}
	}
	backend := m.Backend
	argv := m.Argv
	if m.Tool != "" {
		// A wrapper in /vp/bin. It cannot know which session called it, so the
		// daemon resolves the machine and names the alternatives if it cannot.
		if backend == "" {
			if backend, err = s.resolveTool(session, m.Tool); err != nil {
				_ = k.c.Errorf(m.ID, "%v", err)
				return
			}
		}
		if len(argv) == 0 {
			argv = []string{m.Tool}
		}
	}
	code, err := s.dispatch(dispatchReq{
		session: session, backend: backend, cwd: m.Cwd, argv: argv,
		env: m.Env, tty: m.TTY, files: files, pid: pid,
		onStart: k.trackRemote,
	})
	if err != nil {
		_ = k.c.Errorf(m.ID, "%v", err)
		return
	}
	_ = k.c.Send(&proto.Msg{Op: proto.OpExit, ID: m.ID, Code: code})
}

// trackRemote remembers the running command so that a Ctrl-C arriving on this
// same connection can be forwarded to it. ssh does not forward signals without
// a PTY, and a PTY would merge stderr into stdout, which agents read separately.
func (k *ctlConn) trackRemote(h *remote.Host, id string) {
	k.mu.Lock()
	k.remoteHost, k.remoteExec = h, id
	k.mu.Unlock()
}

func (k *ctlConn) forwardSignal(sig int) {
	k.mu.Lock()
	h, id := k.remoteHost, k.remoteExec
	k.mu.Unlock()
	if h == nil || id == "" {
		return
	}
	if err := h.Signal(id, sig); err != nil {
		k.d.logf("forward signal %d to %s: %v", sig, h.Alias, err)
	}
}

// progress gives a slow request a way to say what it is waiting for.
func (k *ctlConn) progress(id uint64) *Progress {
	return &Progress{id: id, send: func(m *proto.Msg) {
		k.mu.Lock()
		defer k.mu.Unlock()
		_ = k.c.Send(m)
	}}
}

func (k *ctlConn) attached() *session {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.sess
}

func sessionKind(m *proto.Msg) string {
	if m.Session == "console" {
		return "console"
	}
	return "shell"
}

// podOp reports whether an op is allowed on the socket bound into a pod.
//
// The table in DESIGN.md §9, in code. Reading is always allowed. Moving your own
// session is allowed and is the whole interface — v1's tty check on `use` is
// gone, because in v2 an agent choosing its own backend is how work reaches
// another machine at all. Mounting is allowed but allowlisted, because it opens
// a network path out of a sandbox built for containment. Lifecycle is not:
// nothing inside a pod may take it down.
func podOp(op string) bool {
	switch op {
	case proto.OpPs, proto.OpLog, proto.OpTree, proto.OpHosts, proto.OpStat,
		proto.OpBackend, proto.OpBrief, proto.OpUse, proto.OpDispatch,
		proto.OpExec, proto.OpMount, proto.OpUnmount, proto.OpSignal:
		return true
	}
	return false
}

func (d *Daemon) reply(c *proto.Conn, m *proto.Msg, err error) {
	if err != nil {
		_ = c.Errorf(m.ID, "%v", err)
		return
	}
	_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
}

// up creates a pod: its runtime dir, the socket the pod calls back on, every
// mount in the order the config named them, and the namespace itself.
func (d *Daemon) up(m *proto.Msg, pr *Progress) error {
	if m.Spec == nil || m.Spec.Name == "" {
		return fmt.Errorf("up needs a named spec")
	}
	name := m.Spec.Name
	d.mu.Lock()
	if _, exists := d.pods[name]; exists {
		d.mu.Unlock()
		return errAlreadyRunning(name)
	}
	// Someone else is already building it: wait for them rather than making the
	// caller deal with a race it did not cause. Two terminals opening the same
	// project at once is ordinary.
	if ch, busy := d.creating[name]; busy {
		d.mu.Unlock()
		pr.step("pod %s is already being created; waiting… ", name)
		select {
		case <-ch:
		case <-time.After(2 * time.Minute):
			return fmt.Errorf("pod %q is taking too long to start", name)
		}
		d.mu.Lock()
		_, ok := d.pods[name]
		d.mu.Unlock()
		if ok {
			pr.ok("ready")
			return errAlreadyRunning(name)
		}
		return fmt.Errorf("pod %q failed to start; see the daemon log", name)
	}
	done := make(chan struct{})
	d.creating[name] = done
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.creating, name)
		d.mu.Unlock()
		close(done)
	}()

	podRun := filepath.Join(d.runDir, "pods", name)
	// The pod needs two things from its runtime directory: the socket, and the
	// agent brief. Give them their own subdirectory, because the bind that
	// carries it into the pod is recursive — binding the whole directory would
	// also carry in the FUSE mounts under mnt/ and the pod's own root, and a
	// remote directory reachable at a second, unrouted path is a command
	// silently reading the wrong machine's files.
	ctlDir := filepath.Join(podRun, "ctl")
	if err := os.MkdirAll(ctlDir, 0o700); err != nil {
		return err
	}
	sock := filepath.Join(ctlDir, "pod.sock")
	_ = os.Remove(sock)
	ln, err := proto.Listen(sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sock, err)
	}

	m.Spec.RunDir = ctlDir
	m.Spec.Root = filepath.Join(podRun, "root")
	// Where remote mounts are made, and the staging area the pod sees them
	// through. It has to exist before the pod does, because the bind that
	// carries it in happens while the root is being built.
	m.Spec.StageDir = filepath.Join(podRun, "mnt")
	if err := os.MkdirAll(m.Spec.StageDir, 0o700); err != nil {
		ln.Close()
		return err
	}
	if m.Spec.ShellBin == "" {
		if m.Spec.ShellBin, err = shellBinary(); err != nil {
			ln.Close()
			return err
		}
	}
	if m.Spec.CtlBin == "" {
		m.Spec.CtlBin, _ = os.Executable()
	}
	policy := EnvPolicy{Mode: EnvDelta}
	if m.EnvPolicy != nil {
		policy = EnvPolicy{Mode: EnvMode(m.EnvPolicy.Mode), Names: m.EnvPolicy.Names}
		if policy.Mode == "" {
			policy.Mode = EnvDelta
		}
	}

	s := newPodState(d, name, nil, route.New(m.Backend, nil))
	s.envMode = policy
	s.configPath = m.Config
	s.canMount = m.CanMount
	s.toolHosts = m.ToolHosts
	if s.toolHosts == nil {
		s.toolHosts = map[string]string{}
	}
	s.podLn = ln

	// Local directories are binds the pod builds for itself, so they travel in
	// the spec and exist the moment the pod does.
	s.planLocal(m)
	m.Spec.Brief = s.brief()

	p, err := pod.Start(m.Spec)
	if err != nil {
		s.closeFailed(ln)
		return err
	}
	s.p = p
	go s.readLoop()

	// Remote directories are attached afterwards, through the same staging area
	// `vp mount` uses an hour later. One code path deliberately: a mount added at
	// runtime that behaved differently from one in the config would be a second
	// implementation of the only thing this program does.
	if err := s.attachRemotes(m, pr); err != nil {
		p.Kill()
		s.closeFailed(ln)
		return err
	}

	d.mu.Lock()
	d.pods[name] = s
	d.mu.Unlock()

	go d.servePodSocket(s)
	go s.watchExits()
	d.bus.Publish(event.Event{Kind: event.KindPod, Pod: name, Detail: "up",
		PID: p.Pid})
	d.logf("pod %s up (vpinit pid %d, default backend %s)", name, p.Pid,
		s.table().Default)
	return nil
}

// planLocal records the pod's own directories and puts them in the spec, so they
// are there before anything runs.
func (s *podState) planLocal(m *proto.Msg) {
	for _, ms := range m.Mounts {
		if ms.Host != "" {
			continue
		}
		rec := &mountRec{At: ms.At, Src: ms.Src, Owner: route.Pod,
			ExecOn: ms.ExecOn, Kind: "bind", ReadOnly: ms.ReadOnly,
			Identity: ms.Identity}
		s.mu.Lock()
		s.mounts = append(s.mounts, rec)
		s.mu.Unlock()
		m.Spec.Binds = append(m.Spec.Binds,
			proto.Bind{Src: rec.Src, Dst: rec.At, ReadOnly: rec.ReadOnly})
	}
	s.rebuildRoutes()
}

// attachRemotes brings up every remote mount the config asked for and refuses
// the pod if one cannot be reached — at up time, where a person is watching,
// rather than mid-run where an agent would meet it.
func (s *podState) attachRemotes(m *proto.Msg, pr *Progress) error {
	for _, ms := range m.Mounts {
		if ms.Host == "" {
			continue
		}
		rec, err := s.addMount(ms, pr, false)
		if err != nil {
			return err
		}
		if err := s.bindMount(rec); err != nil {
			return fmt.Errorf("bind %s into the pod: %w", rec.At, err)
		}
		s.d.logf("pod %s: mounted %s at %s via %s", s.name, rec.source(),
			rec.At, rec.Kind)
	}
	s.rebuildRoutes()
	return nil
}

// closeFailed releases what a half-built pod had taken.
func (s *podState) closeFailed(ln *net.UnixListener) {
	if s.fs != nil {
		s.fs.Unmount()
	}
	ln.Close()
}

// mount connects a machine and mounts a directory from it into a running pod.
func (d *Daemon) mount(m *proto.Msg, pr *Progress, fromHost bool) error {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return err
	}
	if len(m.Mounts) != 1 {
		return fmt.Errorf("mount takes one host:path")
	}
	ms := m.Mounts[0]
	if ms.Host == "" {
		return fmt.Errorf("mount needs host:/absolute/path")
	}
	if !fromHost {
		if err := s.mountAllowed(ms.Host); err != nil {
			return err
		}
	}
	rec, err := s.addMount(ms, pr, true)
	if err != nil {
		return err
	}
	if err := s.bindMount(rec); err != nil {
		if rec.fsMount != nil && s.fs != nil {
			s.fs.Remove(rec.fsMount)
		}
		s.dropMount(rec)
		return fmt.Errorf("bind %s into the pod: %w", rec.At, err)
	}
	s.rebuildRoutes()
	d.bus.Publish(event.Event{Kind: event.KindPod, Pod: s.name, Target: rec.Owner,
		Detail: "mounted " + rec.At})
	d.logf("pod %s: mounted %s at %s (runtime)", s.name, rec.source(), rec.At)
	return nil
}

func (d *Daemon) unmount(m *proto.Msg) error {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return err
	}
	what := m.Path
	if what == "" {
		what = m.Target
	}
	if what == "" {
		return fmt.Errorf("unmount takes a path in the pod, or @machine")
	}
	return s.removeMount(what)
}

func (d *Daemon) saveReply(c *proto.Conn, m *proto.Msg) {
	s, err := d.lookup(m.Pod)
	if err != nil {
		_ = c.Errorf(m.ID, "%v", err)
		return
	}
	text, err := config.Render(s.saveState())
	if err != nil {
		_ = c.Errorf(m.ID, "%v", err)
		return
	}
	_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Detail: text,
		Path: s.configPath})
}

func (d *Daemon) ps() []proto.PodInfo {
	d.mu.Lock()
	pods := make([]*podState, 0, len(d.pods))
	for _, s := range d.pods {
		pods = append(pods, s)
	}
	d.mu.Unlock()
	out := make([]proto.PodInfo, 0, len(pods))
	for _, s := range pods {
		live := s.live()
		out = append(out, proto.PodInfo{
			Name:     s.name,
			Pid:      s.p.Pid,
			Sessions: len(live),
			Mounts:   len(s.mountList()),
			Uptime:   time.Since(s.started).Truncate(time.Second).String(),
			Default:  s.table().Default,
			Live:     live,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (d *Daemon) down(name string) error {
	d.mu.Lock()
	s, ok := d.pods[name]
	delete(d.pods, name)
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pod %q", name)
	}
	s.close()
	d.logf("pod %s down", name)
	return nil
}

func (d *Daemon) removePod(name string) {
	d.mu.Lock()
	s, ok := d.pods[name]
	delete(d.pods, name)
	d.mu.Unlock()
	if ok {
		s.close()
	}
}

// podOf resolves which pod a request is about. A connection on a pod socket is
// answered from that pod whatever it asks for: the socket is the identity.
func (k *ctlConn) podOf(m *proto.Msg) (*podState, error) {
	if k.pod != nil {
		return k.pod, nil
	}
	return k.d.lookup(m.Pod)
}

func (d *Daemon) lookup(name string) (*podState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if name == "" && len(d.pods) == 1 {
		for _, s := range d.pods {
			return s, nil
		}
	}
	s, ok := d.pods[name]
	if !ok {
		return nil, fmt.Errorf("no pod %q", name)
	}
	return s, nil
}

// errAlreadyRunning is the one error clients treat as success: asking for a pod
// that exists is what every command after the first does.
func errAlreadyRunning(name string) error {
	return fmt.Errorf("pod %q is already running", name)
}

// hostList reports the machines this pod can run commands on, mounted or not.
// It is the answer to "where can I go", which in a pod is a question about
// machines rather than about paths.
func (d *Daemon) hostList(m *proto.Msg) ([]proto.HostInfo, error) {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return nil, err
	}
	dirs := map[string][]string{}
	mounted := map[string]bool{}
	for _, r := range s.table().Rules() {
		dirs[r.Owner] = append(dirs[r.Owner], r.Prefix)
		mounted[r.Owner] = true
		if r.ExecOn != "" {
			mounted[r.ExecOn] = mounted[r.ExecOn] || false
			if _, seen := dirs[r.ExecOn]; !seen {
				dirs[r.ExecOn] = nil
			}
		}
	}
	names := []string{route.Pod}
	for target := range dirs {
		if target != route.Pod {
			names = append(names, target)
		}
	}
	sort.Strings(names[1:])
	// Machines this pod could reach but has not mounted: the answer to "what
	// else is there", which is what `vp mount` needs.
	for _, h := range config.SSHHosts() {
		if _, known := dirs[h]; !known {
			names = append(names, h)
		}
	}

	def := s.table().Default
	out := make([]proto.HostInfo, 0, len(names))
	for _, name := range names {
		h := proto.HostInfo{
			Name:    name,
			Local:   name == route.Pod,
			Mounted: name == route.Pod || len(dirs[name]) > 0,
			Dirs:    dirs[name],
			Default: name == def,
		}
		// An established multiplexed connection answers this instantly; a host
		// with no master yet fails fast rather than dialling out.
		h.Connected = h.Local || d.pool.Host(name).Reachable()
		sort.Strings(h.Dirs)
		out = append(out, h)
	}
	return out, nil
}

// statInPod answers "is this a directory in the pod" without running anything in
// it. /proc/<pid>/root resolves inside that process's mount namespace, so the
// daemon can look through vpinit's eyes.
//
// Running `test -d` in the pod would also work and did, but it put a command
// nobody typed into a log whose whole value is that everything in it was asked
// for.
func (d *Daemon) statInPod(m *proto.Msg) (*proto.Msg, error) {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(m.Path, "/") {
		return nil, fmt.Errorf("stat needs an absolute path")
	}
	// A symlink inside the pod pointing at an absolute path would resolve
	// against our root, not the pod's, so this can say yes to a directory the
	// pod cannot reach. The next command there fails plainly, which is the same
	// answer one step later.
	fi, err := os.Stat(filepath.Join("/proc", strconv.Itoa(s.p.Pid), "root", m.Path))
	code := 1
	if err == nil && fi.IsDir() {
		code = 0
	}
	// Answer the more interesting questions at the same time: whose disk this
	// is, what this session's backend calls it, and which machine the directory
	// itself suggests.
	backend := s.backendOf(m.Session)
	return &proto.Msg{Op: proto.OpOK, Code: code,
		Backend: backend,
		Target:  route.Owner(s.table(), m.Path),
		Detail:  route.Suggest(s.table(), m.Path),
		Path:    route.Dir(s.table(), m.Path, backend)}, nil
}

// runDirect runs a process in the pod on the client's own file descriptors, so
// the daemon never sits in the data path.
func (d *Daemon) runDirect(s *podState, m *proto.Msg, files []*os.File) (int, error) {
	id := m.Session
	if id == "" {
		id = s.nextSessionID()
	}
	backend := s.backendOf(id)
	if m.Backend != "" {
		backend = m.Backend
	}
	env := s.sessionEnv(id, backend, m.Env)
	sess := &session{id: id, kind: "run", argv: m.Argv, env: env, cwd: m.Cwd,
		started: time.Now(), shells: map[string]*shellHandle{},
		clients: map[*attachment]bool{}, done: make(chan int, 1)}
	s.mu.Lock()
	s.sessions[id] = sess
	s.backends[id] = backend
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, id)
		delete(s.backends, id)
		s.mu.Unlock()
	}()

	// A session with no terminal that asked for another machine is a dispatch,
	// which is the same code path every wrapper uses.
	if backend != route.Pod {
		return s.dispatch(dispatchReq{session: id, backend: backend, cwd: m.Cwd,
			argv: m.Argv, env: m.Env, files: files})
	}
	fds := []int{int(files[0].Fd()), int(files[1].Fd()), int(files[2].Fd())}
	return s.runToCompletion(&proto.Msg{
		Op: proto.OpSpawn, Argv: m.Argv, Env: env, Cwd: m.Cwd,
		TTY: m.TTY, Session: id,
	}, fds)
}

// shellBinary finds vpsh next to the running binary.
func shellBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	cand := filepath.Join(filepath.Dir(self), "vpsh")
	if _, err := os.Stat(cand); err != nil {
		return "", fmt.Errorf("vpsh not found next to %s: %w", self, err)
	}
	return cand, nil
}

// sessionEnv is what a session's first process is launched with, and therefore
// the baseline a dispatched command's environment is measured against.
//
// SHELL is the interesting one: it points at vpsh, so that every command an
// agent shells out to is recorded. VIBEPOD_REAL_SHELL is what vpsh then runs,
// because the agent's wrapper is written in that shell's syntax and has to keep
// working exactly as it did.
func (s *podState) sessionEnv(session, backend string, callerEnv []string) []string {
	env := make([]string, 0, len(callerEnv)+8)
	for _, kv := range callerEnv {
		if name, _, ok := strings.Cut(kv, "="); ok {
			switch name {
			case "SHELL", "PATH", "VIBEPOD_POD", "VIBEPOD_SESSION",
				"VIBEPOD_SOCK", "VIBEPOD_BACKEND", "VIBEPOD_REAL_SHELL":
				continue
			}
		}
		env = append(env, kv)
	}
	return append(env,
		"VIBEPOD_POD="+s.name,
		"VIBEPOD_SESSION="+session,
		"VIBEPOD_SOCK="+pod.SockPath,
		// The backend at the moment the session started. It moves, and a process
		// cannot be told afterwards, so `vp backend` is the live answer and this
		// is the one a prompt can afford to read.
		"VIBEPOD_BACKEND="+backend,
		"VIBEPOD_REAL_SHELL="+realShell(callerEnv),
		"SHELL="+pod.ShellPath,
		"PATH="+podPath(callerEnv),
	)
}

// realShell is the shell vpsh hands a command to. It is the user's own, because
// an agent's wrapper is generated in that shell's syntax — zsh setopt, a sourced
// snapshot file — and handing it to /bin/sh would break it in ways that read as
// the agent malfunctioning.
func realShell(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "SHELL="); ok && v != "" &&
			!strings.HasPrefix(v, pod.VpDir+"/") {
			return v
		}
	}
	if sh := os.Getenv("SHELL"); sh != "" && !strings.HasPrefix(sh, pod.VpDir+"/") {
		return sh
	}
	return "/bin/sh"
}

// podPath puts the pod's own tools and the dispatching wrappers first, so a bare
// `vp` works and a wrapped tool is reached before the local binary of that name.
func podPath(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			return pod.BinDir + ":" + v
		}
	}
	return pod.BinDir + ":/usr/bin:/bin"
}

func closeAll(fds []int) {
	for _, fd := range fds {
		syscall.Close(fd)
	}
}
