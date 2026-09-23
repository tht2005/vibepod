// Package daemon is the vibepod user daemon: one per user, rootless,
// auto-spawned. It owns everything long-lived — the pod registry, the exec
// gate for every pod, the route tables, and (from M1) the ssh connections.
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

	"vibepod/internal/event"
	"vibepod/internal/fs"
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
// attached, because a window resize or a detach arrives on this same socket
// and must not queue behind the session it is about.
type ctlConn struct {
	d    *Daemon
	c    *proto.Conn
	host bool // false for connections that arrived on a pod socket

	mu     sync.Mutex
	sess   *session
	detach chan struct{}
}

// handle serves one client. host is false for connections arriving on a pod
// socket, which is what enforces the capability split in DESIGN.md §9: the
// agent can look, but it cannot steer.
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
		if !k.host && !readOnlyOp(m.Op) {
			closeAll(fds)
			_ = k.c.Errorf(m.ID, "%q is not permitted from inside a pod", m.Op)
			continue
		}
		d := k.d
		switch m.Op {
		case proto.OpUp:
			d.reply(k.c, m, d.up(m))
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
		case proto.OpUse:
			d.reply(k.c, m, d.use(m))
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
	// directly on the caller's own descriptors and the daemon stays out of
	// the data path entirely.
	if m.Op == proto.OpSession && !m.TTY {
		code, err := k.d.runDirect(s, m, fds)
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

func readOnlyOp(op string) bool {
	switch op {
	case proto.OpPs, proto.OpLog, proto.OpTree, proto.OpHosts, proto.OpStat:
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

// up creates a pod: its runtime dir, the socket vpsh will call back on, the
// namespace itself, and the gate loop that watches every exec in it.
func (d *Daemon) up(m *proto.Msg) error {
	if m.Spec == nil || m.Spec.Name == "" {
		return fmt.Errorf("up needs a named spec")
	}
	name := m.Spec.Name
	d.mu.Lock()
	if _, exists := d.pods[name]; exists {
		d.mu.Unlock()
		return errAlreadyRunning(name)
	}
	// Someone else is already building it: wait for them rather than making
	// the caller deal with a race it did not cause. Two terminals opening the
	// same project at once is ordinary.
	if ch, busy := d.creating[name]; busy {
		d.mu.Unlock()
		select {
		case <-ch:
		case <-time.After(2 * time.Minute):
			return fmt.Errorf("pod %q is taking too long to start", name)
		}
		d.mu.Lock()
		_, ok := d.pods[name]
		d.mu.Unlock()
		if ok {
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
	// The pod needs exactly one thing from its runtime directory: the socket.
	// Give that its own subdirectory, because the bind that carries it into
	// the pod is recursive — binding the whole directory would also carry in
	// the FUSE mounts under mnt/ and the pod's own root, and a remote
	// directory reachable at a second, unrouted path is a command silently
	// running on the wrong machine.
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
	if m.Spec.ShimBin == "" {
		m.Spec.ShimBin, err = shimBinary()
		if err != nil {
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
	rules := make([]route.Rule, 0, len(m.Routes))
	for _, r := range m.Routes {
		rules = append(rules, route.Rule{Prefix: r.Prefix, Target: r.Target,
			RemotePrefix: r.RemotePrefix})
	}

	// Remote directories are mounted on the host first, then bound in with the
	// rest of the pod's filesystem.
	fsm, err := d.mountRemotes(m, podRun)
	if err != nil {
		ln.Close()
		return err
	}

	p, err := pod.Start(m.Spec)
	if err != nil {
		if fsm != nil {
			fsm.Unmount()
		}
		ln.Close()
		return err
	}
	s := newPodState(d, name, p, route.New(m.ExecDefault, rules), m.ShimAll)
	s.envMode = policy
	s.podLn = ln
	s.fs = fsm

	d.mu.Lock()
	d.pods[name] = s
	d.mu.Unlock()

	go s.readLoop()
	go d.runGate(s)
	go d.servePodSocket(s)
	go s.watchExits()
	d.bus.Publish(event.Event{Kind: event.KindPod, Pod: name, Detail: "up",
		PID: p.Pid})
	d.logf("pod %s up (vpinit pid %d)", name, p.Pid)
	return nil
}

// mountRemotes brings up every remote mount this pod needs, and refuses the
// pod if any of them cannot be reached — at up time, where a person is
// watching, rather than mid-run where an agent would meet it.
func (d *Daemon) mountRemotes(m *proto.Msg, podRun string) (*fs.Manager, error) {
	if len(m.Remotes) == 0 {
		return nil, nil
	}
	backend, err := fs.Pick()
	if err != nil {
		return nil, err
	}
	fsm := fs.NewManager(backend)
	for i, rm := range m.Remotes {
		if rm.Mode != "" && rm.Mode != "fuse" {
			fsm.Unmount()
			return nil, fmt.Errorf("mount mode %q is not implemented yet", rm.Mode)
		}
		host := d.pool.Host(rm.Host)
		if err := host.Warm(); err != nil {
			fsm.Unmount()
			return nil, err
		}
		point := filepath.Join(podRun, "mnt", fmt.Sprintf("%d", i))
		mount := &fs.Mount{
			Host: rm.Host, RemotePath: rm.Path, MountPoint: point,
			At: rm.At, ReadOnly: rm.ReadOnly,
		}
		if err := fsm.Add(mount, host.SSHCommand()); err != nil {
			fsm.Unmount()
			return nil, err
		}
		m.Spec.Binds = append(m.Spec.Binds,
			proto.Bind{Src: point, Dst: rm.At, ReadOnly: rm.ReadOnly})
		d.logf("pod %s: mounted %s:%s at %s via %s", m.Spec.Name, rm.Host, rm.Path,
			rm.At, backend.Name())
	}
	return fsm, nil
}

func (d *Daemon) ps() []proto.PodInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]proto.PodInfo, 0, len(d.pods))
	for _, s := range d.pods {
		s.mu.Lock()
		n := len(s.sessions)
		s.mu.Unlock()
		out = append(out, proto.PodInfo{
			Name:     s.name,
			Pid:      s.p.Pid,
			Sessions: n,
			Mounts:   len(s.p.Spec.Binds),
			Uptime:   time.Since(s.started).Truncate(time.Second).String(),
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

// errAlreadyRunning is the one error clients treat as success: asking for a
// pod that exists is what `vpctl run` does every time after the first.
func errAlreadyRunning(name string) error {
	return fmt.Errorf("pod %q is already running", name)
}

// hostList reports the machines this pod runs commands on and the directories
// that belong to each. It is the answer to "where can I go", which in a pod is
// a question about machines rather than about paths.
func (d *Daemon) hostList(m *proto.Msg) ([]proto.HostInfo, error) {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return nil, err
	}
	dirs := map[string][]string{}
	for _, r := range s.table.Rules() {
		dirs[r.Target] = append(dirs[r.Target], r.Prefix)
	}
	names := []string{route.Pod}
	for target := range dirs {
		if target != route.Pod {
			names = append(names, target)
		}
	}
	sort.Strings(names[1:])

	out := make([]proto.HostInfo, 0, len(names))
	for _, name := range names {
		h := proto.HostInfo{
			Name:    name,
			Local:   name == route.Pod,
			Dirs:    dirs[name],
			Default: name == s.table.Default,
		}
		// An established multiplexed connection answers this instantly; a
		// host with no master yet fails fast rather than dialling out.
		h.Connected = h.Local || d.pool.Host(name).Reachable()
		sort.Strings(h.Dirs)
		out = append(out, h)
	}
	return out, nil
}

// statInPod answers "is this a directory in the pod" without running anything
// in it. /proc/<pid>/root resolves inside that process's mount namespace, so
// the daemon can look through vpinit's eyes.
//
// Running `test -d` in the pod would also work and did, but it put a command
// nobody typed into a log whose whole value is that everything in it was
// asked for.
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
	// pod cannot reach. The next command there fails plainly, which is the
	// same answer one step later.
	fi, err := os.Stat(filepath.Join("/proc", strconv.Itoa(s.p.Pid), "root", m.Path))
	code := 1
	if err == nil && fi.IsDir() {
		code = 0
	}
	// Answer the more interesting question at the same time: which machine
	// owns this directory, and what is it called there. The daemon's own route
	// table decides, so there is only ever one resolver.
	dec := route.Resolve(s.table, m.Path, s.pinOf(m.Session))
	return &proto.Msg{Op: proto.OpOK, Code: code, Target: dec.Target,
		Path: dec.Dir}, nil
}

// use pins a session's executor. This is the sharp edge of the in-pod
// control plane: an agent that can pin its own executor can send itself to a
// machine its directory would never have chosen, granting itself a capability
// nobody gave it. The tty check is a proxy for intent, not proof of it — but
// it is cheap, it fails closed, and a human at a terminal has one where an
// agent's subprocess does not.
func (d *Daemon) use(m *proto.Msg) error {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return err
	}
	if m.Session == "" {
		return fmt.Errorf("use applies to a session; none was named")
	}
	s.mu.Lock()
	if m.Target == "" || m.Target == "auto" {
		delete(s.pins, m.Session)
	} else {
		s.pins[m.Session] = m.Target
	}
	s.mu.Unlock()
	d.logf("pod %s: session %s now runs on %s", s.name, m.Session, m.Target)
	return nil
}

// runDirect runs a process in the pod on the client's own file descriptors,
// so the daemon never sits in the data path.
func (d *Daemon) runDirect(s *podState, m *proto.Msg, fds []int) (int, error) {
	id := m.Session
	if id == "" {
		id = s.nextSessionID()
	}
	sess := &session{id: id, kind: "run", argv: m.Argv, done: make(chan int, 1)}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
	}()

	env := sessionEnv(s.name, id, m.Env)
	sess.env = env
	s.claimSession(id)
	reply, err := s.call(&proto.Msg{
		Op: proto.OpSpawn, Argv: m.Argv, Env: env, Cwd: m.Cwd,
		TTY: m.TTY, Session: id,
	}, fds[0], fds[1], fds[2])
	if err != nil {
		return 0, err
	}
	sess.pid = reply.Pid
	return <-sess.done, nil
}

// shimBinary finds vpsh next to the running binary.
func shimBinary() (string, error) {
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
// the baseline a routed command's environment is measured against.
func sessionEnv(podName, session string, callerEnv []string) []string {
	env := append([]string{}, callerEnv...)
	return append(env,
		"VIBEPOD_POD="+podName,
		"VIBEPOD_SESSION="+session,
		"VIBEPOD_SOCK="+pod.SockPath,
		"PATH="+podPath(callerEnv),
	)
}

// podPath puts the pod's own tools first, so an agent can run `vpctl tree`
// without being told where it lives.
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
