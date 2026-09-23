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
	"sync"
	"syscall"
	"time"

	"vibepod/internal/pod"
	"vibepod/internal/proto"
	"vibepod/internal/route"
)

type Daemon struct {
	runDir string
	logger *log.Logger

	mu   sync.Mutex
	pods map[string]*podState
}

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

func New(runDir string, logger *log.Logger) *Daemon {
	return &Daemon{runDir: runDir, logger: logger, pods: map[string]*podState{}}
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
}

// handle serves one client. host is false for connections arriving on a pod
// socket, which is what enforces the capability split in DESIGN.md §9: the
// agent can look, but it cannot steer.
func (d *Daemon) handle(c *proto.Conn, host bool) {
	defer c.Close()
	for {
		m, fds, err := c.Recv()
		if err != nil {
			closeAll(fds)
			return
		}
		if !host && !readOnlyOp(m.Op) {
			closeAll(fds)
			_ = c.Errorf(m.ID, "%q is not permitted from inside a pod", m.Op)
			continue
		}
		switch m.Op {
		case proto.OpUp:
			d.reply(c, m, d.up(m))
		case proto.OpPs:
			_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Pods: d.ps()})
		case proto.OpDown:
			d.reply(c, m, d.down(m.Pod))
		case proto.OpSession:
			code, err := d.session(m, fds)
			closeAll(fds)
			if err != nil {
				_ = c.Errorf(m.ID, "%v", err)
			} else {
				_ = c.Send(&proto.Msg{Op: proto.OpExit, ID: m.ID, Code: code})
			}
		default:
			closeAll(fds)
			_ = c.Errorf(m.ID, "unknown op %q", m.Op)
		}
	}
}

func readOnlyOp(op string) bool {
	return op == proto.OpPs
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
	_, exists := d.pods[name]
	d.mu.Unlock()
	if exists {
		return fmt.Errorf("pod %q is already running", name)
	}

	podRun := filepath.Join(d.runDir, "pods", name)
	if err := os.MkdirAll(podRun, 0o700); err != nil {
		return err
	}
	sock := filepath.Join(podRun, "pod.sock")
	_ = os.Remove(sock)
	ln, err := proto.Listen(sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sock, err)
	}

	m.Spec.RunDir = podRun
	m.Spec.Root = filepath.Join(podRun, "root")
	if m.Spec.ShimBin == "" {
		m.Spec.ShimBin, err = shimBinary()
		if err != nil {
			ln.Close()
			return err
		}
	}
	rules := make([]route.Rule, 0, len(m.Routes))
	for _, r := range m.Routes {
		rules = append(rules, route.Rule{Prefix: r.Prefix, Target: r.Target})
	}

	p, err := pod.Start(m.Spec)
	if err != nil {
		ln.Close()
		return err
	}
	s := newPodState(d, name, p, route.New(m.ExecDefault, rules), m.ShimAll)
	s.podLn = ln

	d.mu.Lock()
	d.pods[name] = s
	d.mu.Unlock()

	go s.readLoop()
	go d.runGate(s)
	go d.servePodSocket(s)
	d.logf("pod %s up (vpinit pid %d)", name, p.Pid)
	return nil
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

// session runs a process in the pod on the client's own file descriptors, so
// the daemon never sits in the data path.
func (d *Daemon) session(m *proto.Msg, fds []int) (int, error) {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return 0, err
	}
	if len(fds) < 3 {
		return 0, fmt.Errorf("a session needs stdin, stdout and stderr")
	}
	id := m.Session
	if id == "" {
		id = fmt.Sprintf("s%d", time.Now().UnixNano()%100000)
	}
	sess := &session{id: id, argv: m.Argv, done: make(chan int, 1)}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
	}()

	env := append([]string{}, m.Env...)
	env = append(env,
		"VIBEPOD_POD="+s.name,
		"VIBEPOD_SESSION="+id,
		"VIBEPOD_SOCK="+pod.SockPath,
	)
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

func closeAll(fds []int) {
	for _, fd := range fds {
		syscall.Close(fd)
	}
}
