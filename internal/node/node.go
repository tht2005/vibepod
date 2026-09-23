// Package node is vibepod on a machine that is not yours.
//
// It is `vpinit` with the identity plane omitted, and that omission is the point:
// a node pod is a filesystem, not an agent. No credentials are pushed, nothing is
// installed but one binary in ~/.vp/bin, and the pod holds the same composed zone
// as the pod on your own machine — the same paths, in the same order.
//
// Why a pod on the node at all, rather than translating paths for each command:
// DESIGN.md §6. The short version is that rewriting cwd and argv gets the easy
// paths and misses the ones inside a YAML file, built at run time in Python, or
// written down for a later job — an unbounded list, which is exactly what the
// exec gate was deleted for. A path that is *the same string everywhere* needs no
// translation and cannot leak.
//
// The asymmetry with the local pod is deliberate. Here the pod is built as the
// node's **own** system layer with the composed zone over it, not as a minimal
// root: gpu05's /opt/rocm and /dev/kfd are the reason to dispatch there at all,
// and a minimal root would hide the GPUs and read as "ROCm is broken".
package node

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"vibepod/internal/fs"
	"vibepod/internal/pod"
	"vibepod/internal/proto"
	"vibepod/internal/remote"
	"vibepod/internal/sys"
	"vibepod/internal/term"
)

// Dir is where vibepod keeps everything it has on a node: one binary, and state.
// Nothing outside it is written, and `vp unmount`/`down` removes the state.
const Dir = "~/.vp"

// Paths on a node, relative to $HOME.
const (
	binRel = ".vp/bin"
	runRel = ".vp/run"
)

// BinPath is where the pushed binary lives on a node.
func BinPath() string { return filepath.Join(binRel, "vibepod") }

// RunDir is a node pod's state directory, as the node sees it.
func RunDir(home, pod string) string { return filepath.Join(home, runRel, pod) }

// SockPath is the socket a node pod is reached on.
func SockPath(home, pod string) string { return filepath.Join(RunDir(home, pod), "node.sock") }

// server is a node pod and the requests it answers.
type server struct {
	spec *proto.NodeSpec
	p    *pod.Pod
	fsm  *fs.Manager
	ln   *net.UnixListener

	mu    sync.Mutex
	waits map[int]chan int
	// held is what this pod has, in the order it was added. The daemon reconciles
	// against it: it is the reconciler's only input from this side.
	held []proto.Held
	// seq names the next mountpoint. Indexed rather than named after the host,
	// because several mounts from one machine is ordinary.
	seq int
	// leaseUntil is when this pod gives up on ever hearing from the daemon again
	// and takes itself down. Zero means no lease was asked for.
	leaseUntil time.Time
	lease      time.Duration
	// expired is set when the lease ran out, which is the one shutdown with nobody
	// on the other end to clear this pod's state afterwards — so it clears its own.
	expired bool

	rpcMu   sync.Mutex
	nextID  uint64
	pending map[uint64]chan rpcReply
}

type rpcReply struct {
	msg *proto.Msg
	fds []int
}

// Run is `vibepod vpnode`: build the pod, then answer spawn requests until the
// daemon that started it says otherwise.
//
// It is started over ssh and must outlive that ssh, so the parent process here
// forks a child, waits for it to say the pod exists, reports that and exits. A
// failure is therefore reported to the person running `up`, on the connection
// they are already watching, rather than into a log file on a machine they would
// have to go and read.
func Run(args []string) error {
	specPath, child := "", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--spec":
			if i+1 < len(args) {
				i++
				specPath = args[i]
			}
		case "--child":
			child = true
		}
	}
	if specPath == "" {
		return fmt.Errorf("vpnode needs --spec <file>")
	}
	if !child {
		return spawnChild(specPath)
	}
	return serve(specPath)
}

// spawnChild starts the real vpnode and waits for it to say it is up.
func spawnChild(specPath string) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "vpnode", "--spec", specPath, "--child")
	cmd.ExtraFiles = []*os.File{w} // fd 3 in the child
	cmd.Stdin = nil
	logPath := filepath.Join(filepath.Dir(specPath), "node.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start vpnode: %w", err)
	}
	w.Close()
	_ = cmd.Process.Release()

	// Bounded, because a child that says nothing must still report something: on a
	// node there is nobody to notice a wait that never ends.
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- strings.TrimSpace(string(out))
	}()
	var line string
	select {
	case line = <-done:
	case <-time.After(3 * time.Minute):
		r.Close()
		return fmt.Errorf("the node pod did not report back in three minutes; "+
			"see %s on this machine", logPath)
	}
	r.Close()
	if line == "" {
		return fmt.Errorf("the node pod did not start; see %s on this machine", logPath)
	}
	if rest, ok := strings.CutPrefix(line, "ready "); ok {
		fmt.Println("ready " + rest)
		return nil
	}
	return fmt.Errorf("%s", strings.TrimPrefix(line, "error "))
}

// serve builds the pod and answers requests. It is the child.
func serve(specPath string) error {
	ready := os.NewFile(3, "ready")
	// Close-on-exec, or the parent never learns that this pod is up.
	//
	// The parent waits for EOF on this pipe. An inherited descriptor does not carry
	// that flag, so without this it is passed on to everything spawned from here —
	// and sshfs *daemonizes*, so it holds the write end open for the life of the
	// mount and the parent waits forever on a pipe that will never close. The
	// symptom is an `up` that hangs after everything has already worked.
	syscall.CloseOnExec(3)
	fail := func(err error) error {
		if ready != nil {
			fmt.Fprintf(ready, "error %v\n", err)
			ready.Close()
		}
		return err
	}
	b, err := os.ReadFile(specPath)
	if err != nil {
		return fail(err)
	}
	var spec proto.NodeSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return fail(fmt.Errorf("read the spec: %w", err))
	}
	// A backend pushed into ~/.vp/bin is not on anyone's PATH. Put it there for
	// this process only: nothing about the node's own environment is changed.
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.Setenv("PATH", filepath.Join(home, binRel)+":"+os.Getenv("PATH"))
	}
	s := &server{spec: &spec, waits: map[int]chan int{},
		pending: map[uint64]chan rpcReply{}}

	sock := filepath.Join(spec.RunDir, "node.sock")
	// One pod of a given name per machine. If something is answering, this is a
	// second request for a pod that already exists and saying so is the answer; if
	// nothing is, a previous one died and left its mounts, and those have to go
	// before anything can be built.
	//
	// They have to go for two reasons: a mountpoint whose server is gone fails
	// every access until it is detached, and the staging bind below is refused
	// outright by the kernel while the directory it binds has mounts underneath
	// that this namespace did not make.
	if c, err := proto.Dial(sock); err == nil {
		c.Close()
		return fail(fmt.Errorf("a pod called %q is already running on this machine",
			spec.Pod))
	}
	// Kill first, then unmount, then clear. A previous pod's processes pin its
	// mounts — a bind inside a namespace keeps the filesystem busy, and unmounting
	// out here does not release it while that namespace lives — which is the same
	// order `down` uses, for the same reason.
	killStale(spec.RunDir)
	fs.UnmountUnder(spec.RunDir)
	_ = os.RemoveAll(filepath.Join(spec.RunDir, "root"))
	_ = os.RemoveAll(filepath.Join(spec.RunDir, "mnt"))
	if err := os.MkdirAll(spec.RunDir, 0o700); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(spec.RunDir, "vpnode.pid"),
		[]byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return fail(err)
	}

	podSpec := &proto.Spec{
		Name:     spec.Pod,
		Root:     filepath.Join(spec.RunDir, "root"),
		RunDir:   filepath.Join(spec.RunDir, "ctl"),
		StageDir: filepath.Join(spec.RunDir, "mnt"),
		Hostname: spec.Hostname,
		// No agent brief and no wrappers: a node pod has no agent in it. What it
		// has is the composed zone, at the same paths as everywhere else.
	}
	for _, d := range []string{podSpec.RunDir, podSpec.StageDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fail(err)
		}
	}
	// The node's own directories are native binds: no FUSE, no cache, no round
	// trip. Running work where the data lives is then full speed with nothing to
	// configure, which is the property that makes "mount both, dispatch to
	// either" worth having.
	var remote []proto.MountSpec
	for _, m := range spec.Mounts {
		if m.Identity {
			continue // never; there is no identity plane on a node
		}
		if m.Host == spec.Node {
			podSpec.Binds = append(podSpec.Binds,
				proto.Bind{Src: m.Path, Dst: m.At, ReadOnly: m.ReadOnly})
			// Recorded, or the reconciler sees it missing and tries to add it again.
			s.remember(proto.Held{At: m.At, Host: m.Host, Path: m.Path, Native: true,
				ReadOnly: m.ReadOnly, Generation: m.Generation})
			continue
		}
		remote = append(remote, m)
	}
	p, err := pod.Start(podSpec)
	if err != nil {
		return fail(err)
	}
	s.p = p
	go s.readLoop()

	if len(remote) > 0 {
		if err := s.mountRemote(remote); err != nil {
			p.Kill()
			return fail(err)
		}
	}

	_ = os.Remove(sock)
	ln, err := proto.Listen(sock)
	if err != nil {
		p.Kill()
		return fail(fmt.Errorf("listen %s: %w", sock, err))
	}
	s.mu.Lock()
	s.ln = ln
	if spec.Lease != "" {
		if d, err := time.ParseDuration(spec.Lease); err == nil && d > 0 {
			s.lease = d
			s.leaseUntil = time.Now().Add(d)
		}
	}
	s.mu.Unlock()
	if s.lease > 0 {
		go s.leaseWatch()
	}
	fmt.Fprintf(ready, "ready %s\n", sock)
	ready.Close()
	ready = nil

	defer func() {
		ln.Close()
		// The pod's processes go first, so nothing is still writing; closing their
		// files is also what starts write-back's last uploads.
		p.Kill()
		if s.fsm != nil {
			// Then wait for those uploads, whatever the reason for stopping — `down`,
			// `node drop`, or an expired lease. Unmounting first would discard a
			// checkpoint that exists nowhere else, which is the worst thing this
			// program could do; if a flush fails the mount is still released, but
			// the log says what was lost rather than nothing.
			for _, m := range s.fsm.Mounts() {
				if m.ReadOnly {
					continue
				}
				if err := s.fsm.Flush(m, 5*time.Minute); err != nil {
					fmt.Printf("flush %s before stopping: %v\n", m.At, err)
				}
			}
			s.fsm.Unmount()
		}
		// An expired lease means the daemon is gone, so nothing will run `nodedown`
		// to clear this directory. Nothing is left on a machine vibepod has stopped
		// talking to except the binary it was allowed to put there.
		s.mu.Lock()
		expired := s.expired
		s.mu.Unlock()
		if expired {
			fs.UnmountUnder(spec.RunDir)
			_ = os.RemoveAll(spec.RunDir)
		}
	}()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			return nil
		}
		go s.handle(proto.NewConn(c))
	}
}

// cacheOf is how much of this machine's disk a mount may use. Per mount, because
// a dataset and a checkpoint directory are not the same size of thing.
func cacheOf(m proto.MountSpec, spec *proto.NodeSpec) string {
	if m.Cache != "" {
		return m.Cache
	}
	return spec.Cache
}

// killStale ends a node pod whose socket is dead but whose processes are not.
func killStale(runDir string) {
	b, err := os.ReadFile(filepath.Join(runDir, "vpnode.pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 || pid == os.Getpid() {
		return
	}
	// vpinit goes with it: it is started with Pdeathsig, and the namespace it
	// holds — with every mount inside it — ends when it does.
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(100 * time.Millisecond)
}

// mountRemote brings up the mounts this node does not own, on the node's own
// disk, and binds them into its pod.
//
// FUSE cannot be mounted from inside the pod — setuid is inert there and
// fusermount3 is setuid — so it is mounted here, in the node's root namespace,
// and bound in. That is the same lesson the local pod already learned, one level
// out.
func (s *server) mountRemote(mounts []proto.MountSpec) error {
	backend, err := fs.Pick()
	if err != nil {
		return fmt.Errorf("on %s: %w", s.spec.Node, err)
	}
	s.fsm = fs.NewManager(backend)
	fmt.Printf("mounting %d remote(s) with %q\n", len(mounts), s.sshCmd())
	for _, m := range mounts {
		if err := s.addMount(m); err != nil {
			return err
		}
	}
	return nil
}

// sshCmd is what this node's own outbound ssh looks like. The same options every
// vibepod ssh uses: on a node there is nobody to answer a prompt, so a mount that
// could wait for one would hang with no symptom at all.
func (s *server) sshCmd() string {
	return "ssh " + strings.Join(append(remote.BaseOpts(), s.spec.SSHExtra...), " ")
}

// addMount brings up one mount and binds it into the pod. The same code at build
// time and an hour later, which is what lets a pod here converge to a mount list
// that changed on the machine that owns the agent.
func (s *server) addMount(m proto.MountSpec) error {
	if m.Host == s.spec.Node {
		return s.addOwn(m)
	}
	if s.fsm == nil {
		backend, err := fs.Pick()
		if err != nil {
			return fmt.Errorf("on %s: %w", s.spec.Node, err)
		}
		s.fsm = fs.NewManager(backend)
	}
	s.mu.Lock()
	s.seq++
	point := filepath.Join(s.spec.RunDir, "mnt", fmt.Sprintf("%d", s.seq))
	s.mu.Unlock()

	mount := &fs.Mount{Host: m.Host, RemotePath: m.Path, MountPoint: point,
		At: m.At, ReadOnly: m.ReadOnly, Cache: cacheOf(m, s.spec),
		Prefetch: m.Prefetch}
	if err := s.fsm.Add(mount, s.sshCmd()); err != nil {
		return fmt.Errorf("%s cannot reach %s:%s: %w", s.spec.Node, m.Host, m.Path,
			err)
	}
	if _, err := s.call(&proto.Msg{Op: proto.OpBind,
		Src: pod.StagedPath(point), Dst: m.At, ReadOnly: m.ReadOnly}); err != nil {
		s.fsm.Remove(mount)
		return fmt.Errorf("bind %s into the node pod: %w", m.At, err)
	}
	s.remember(proto.Held{At: m.At, Host: m.Host, Path: m.Path,
		ReadOnly: m.ReadOnly, Generation: m.Generation})
	fmt.Printf("%s:%s is at %s\n", m.Host, m.Path, m.At)
	// Opt-in, and only for the shape a lazy cache handles badly: a run that
	// touches one percent of a tree should not pay for all of it.
	switch did, err := s.fsm.Prefetch(mount, s.sshCmd()); {
	case err != nil:
		fmt.Printf("prefetch %s: %v\n", m.At, err)
	case did:
		fmt.Printf("prefetched %s onto local disk\n", m.At)
	case m.Prefetch:
		fmt.Printf("prefetch %s: %s cannot, so the first pass reads over the "+
			"network\n", m.At, s.fsm.Backend())
	}
	return nil
}

// addOwn brings one of this machine's own directories into a pod that already
// exists.
//
// At build time these are plain binds, made before pivot_root while the node's
// filesystem is still in view. Afterwards there is no way to bind one in: the
// kernel refuses a bind whose source is in another mount namespace, and this
// process has no privilege to place one in the staging area the way a FUSE mount
// arrives. So it goes through a FUSE hop to the node's own sftp-server — no ssh,
// no network, no credentials — and arrives by propagation like any other mount.
// Slower than a bind, and said so; `vp node drop` and `add` makes it native.
func (s *server) addOwn(m proto.MountSpec) error {
	if s.fsm == nil {
		backend, err := fs.Pick()
		if err != nil {
			return fmt.Errorf("on %s: %w", s.spec.Node, err)
		}
		s.fsm = fs.NewManager(backend)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.seq++
	point := filepath.Join(s.spec.RunDir, "mnt", fmt.Sprintf("%d", s.seq))
	s.mu.Unlock()
	mount := &fs.Mount{Host: "localhost", RemotePath: m.Path, MountPoint: point,
		At: m.At, ReadOnly: m.ReadOnly, LocalHop: true}
	if err := s.fsm.Add(mount, self+" sftp-local"); err != nil {
		return fmt.Errorf("%s: mount its own %s: %w", s.spec.Node, m.Path, err)
	}
	if _, err := s.call(&proto.Msg{Op: proto.OpBind,
		Src: pod.StagedPath(point), Dst: m.At, ReadOnly: m.ReadOnly}); err != nil {
		s.fsm.Remove(mount)
		return fmt.Errorf("bind %s into the node pod: %w", m.At, err)
	}
	s.remember(proto.Held{At: m.At, Host: m.Host, Path: m.Path, Native: true,
		Hop: true, ReadOnly: m.ReadOnly, Generation: m.Generation})
	fmt.Printf("%s is this machine's own; added after the pod was built, so it goes "+
		"through a local FUSE hop until the pod is rebuilt\n", m.Path)
	return nil
}

// SFTPLocal is `vibepod sftp-local`: the far end of that hop. sshfs and rclone run
// their "ssh command" with ssh's arguments and then speak sftp on its stdio; this
// ignores the arguments and becomes the machine's own sftp-server. Nothing leaves
// the machine and nothing authenticates, because nothing needs to.
func SFTPLocal() error {
	for _, c := range []string{"/usr/lib/ssh/sftp-server", "/usr/libexec/sftp-server",
		"/usr/lib/openssh/sftp-server", "/usr/libexec/openssh/sftp-server"} {
		if _, err := os.Stat(c); err == nil {
			return syscall.Exec(c, []string{c}, os.Environ())
		}
	}
	return fmt.Errorf("no sftp-server on this machine")
}

// removeMount takes one mount out of this pod.
//
// Flush first, and refuse if it cannot be flushed. Write-back caching means this
// machine may be holding a checkpoint that exists nowhere else, and discarding one
// silently would be the worst bug in this program.
func (s *server) removeMount(at string) error {
	s.mu.Lock()
	var found *proto.Held
	for i := range s.held {
		if s.held[i].At == at {
			found = &s.held[i]
		}
	}
	s.mu.Unlock()
	if found == nil {
		return fmt.Errorf("%s is not mounted in the pod on %s", at, s.spec.Node)
	}
	var mount *fs.Mount
	if s.fsm != nil {
		for _, m := range s.fsm.Mounts() {
			if m.At == at {
				mount = m
			}
		}
	}
	if mount != nil && !mount.ReadOnly {
		if err := s.fsm.Flush(mount, 30*time.Second); err != nil {
			return fmt.Errorf("%s still has unwritten data for %s: %w", s.spec.Node,
				at, err)
		}
	}
	if _, err := s.call(&proto.Msg{Op: proto.OpUnbind, Dst: at}); err != nil {
		return fmt.Errorf("unbind %s in the node pod: %w", at, err)
	}
	if mount != nil {
		s.fsm.Remove(mount)
	}
	s.mu.Lock()
	for i := range s.held {
		if s.held[i].At == at {
			s.held = append(s.held[:i], s.held[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	fmt.Printf("released %s\n", at)
	return nil
}

func (s *server) remember(h proto.Held) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = append(s.held, h)
}

func (s *server) heldList() []proto.Held {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]proto.Held(nil), s.held...)
}

// invalidate drops what this machine cached for a mount, because a command just
// finished somewhere else and those bytes may no longer be current.
//
// One cache became N when every backend got a pod. Execution-aware invalidation
// was built for one copy, where forgetting a whole host's mounts was harmless;
// with several holders it has to reach each of them, and per mount rather than per
// host so an unrelated mount's cache is not thrown away for nothing.
func (s *server) invalidate(at string) {
	if s.fsm == nil {
		return
	}
	for _, m := range s.fsm.Mounts() {
		if at != "" && m.At != at {
			continue
		}
		s.fsm.InvalidateMount(m)
	}
}

// renew pushes the lease out. Called whenever the daemon says anything at all: a
// pod that is being used is a pod somebody still wants.
func (s *server) renew() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lease > 0 {
		s.leaseUntil = time.Now().Add(s.lease)
	}
}

// leaseWatch takes this pod down when the daemon has been gone too long.
//
// Both obvious answers are wrong. Dying with the ssh channel kills an eight-hour
// training run because somebody shut a laptop lid; living forever leaves caches and
// a namespace on a machine other people share, with nobody who remembers why. A
// lease does neither: the run survives a disconnect, and an abandoned pod cleans up
// after itself. On expiry it flushes first — the bytes it is holding may exist
// nowhere else.
func (s *server) leaseWatch() {
	for {
		s.mu.Lock()
		check := s.lease / 4
		s.mu.Unlock()
		if check > 15*time.Second {
			check = 15 * time.Second
		}
		if check < 100*time.Millisecond {
			check = 100 * time.Millisecond
		}
		time.Sleep(check)
		s.mu.Lock()
		until, lease := s.leaseUntil, s.lease
		s.mu.Unlock()
		if lease <= 0 || until.IsZero() || time.Now().Before(until) {
			continue
		}
		fmt.Printf("the daemon has not been heard from in %s; flushing and "+
			"shutting down\n", lease)
		if s.fsm != nil {
			for _, m := range s.fsm.Mounts() {
				if m.ReadOnly {
					continue
				}
				if err := s.fsm.Flush(m, 2*time.Minute); err != nil {
					fmt.Printf("flush %s: %v\n", m.At, err)
				}
			}
		}
		s.mu.Lock()
		s.expired = true
		s.mu.Unlock()
		s.stop()
		return
	}
}

// stop tears the pod down. The listener's Close makes serve's accept loop
// return, and its defer does the rest.
func (s *server) stop() {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

// handle answers one request from the daemon's side of the link, which arrives as
// an ssh running `vibepod nodeexec`.
func (s *server) handle(c *proto.Conn) {
	defer c.Close()
	for {
		m, fds, err := c.Recv()
		if err != nil {
			closeAll(fds)
			return
		}
		switch m.Op {
		case proto.OpSpawn:
			code, err := s.spawn(c, m, fds)
			if err != nil {
				_ = c.Send(&proto.Msg{Op: proto.OpErr, ID: m.ID, Err: err.Error()})
				continue
			}
			_ = c.Send(&proto.Msg{Op: proto.OpExit, ID: m.ID, Code: code})
		case proto.OpSignal:
			s.mu.Lock()
			_, tracked := s.waits[m.Pid]
			s.mu.Unlock()
			if tracked {
				_ = syscall.Kill(-m.Pid, syscall.Signal(m.Sig))
			}
			_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
		case proto.OpDown:
			// The daemon says this node pod is finished. Closing our side of the
			// listener is what actually tears it down; see serve's defer.
			closeAll(fds)
			_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
			s.stop()
			return
		case proto.OpHeld:
			closeAll(fds)
			_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Held: s.heldList(),
				Version: Version, Path: s.spec.Pod})
		case proto.OpMount:
			closeAll(fds)
			if len(m.Mounts) != 1 {
				_ = c.Send(&proto.Msg{Op: proto.OpErr, ID: m.ID,
					Err: "mount takes one mount"})
				break
			}
			if err := s.addMount(m.Mounts[0]); err != nil {
				_ = c.Send(&proto.Msg{Op: proto.OpErr, ID: m.ID, Err: err.Error()})
				break
			}
			_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Held: s.heldList()})
		case proto.OpUnmount:
			closeAll(fds)
			if err := s.removeMount(m.Path); err != nil {
				_ = c.Send(&proto.Msg{Op: proto.OpErr, ID: m.ID, Err: err.Error()})
				break
			}
			_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Held: s.heldList()})
		case proto.OpInvalidate:
			closeAll(fds)
			s.invalidate(m.Path)
			_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
		case proto.OpLease:
			// The daemon is still there. Renewing is the only thing that keeps this
			// pod alive: see leaseWatch for what happens when it stops.
			closeAll(fds)
			s.renew()
			_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
		case proto.OpPs:
			closeAll(fds)
			_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Version: Version,
				Path: s.spec.Pod})
		default:
			closeAll(fds)
			_ = c.Send(&proto.Msg{Op: proto.OpErr, ID: m.ID,
				Err: fmt.Sprintf("%q is not something a node pod does", m.Op)})
		}
	}
}

// spawn runs one command in the node pod and waits for it.
//
// Piped, the caller's own descriptors go straight to the process: they came over
// ssh and then over a unix socket, so the output goes back to whoever asked for it
// and nothing sits in the data path.
//
// Interactive, the pod allocates a terminal of its own and the master is handed
// *back* to the caller, which relays. The caller's stdin is a pty allocated by
// ssh, and that one is already another session's controlling terminal — so a
// process here cannot adopt it, and a shell without a controlling terminal has no
// job control. One pty per side, joined by a copy, is what makes `fg` work on a
// machine two hops away.
func (s *server) spawn(c *proto.Conn, m *proto.Msg, fds []int) (int, error) {
	defer closeAll(fds)
	if len(fds) < 3 {
		return 0, fmt.Errorf("a command needs stdin, stdout and stderr")
	}
	wait := make(chan int, 1)
	// The caller's directory is the node's own home when the command came from a
	// directory no pod holds, and a node pod holds only the composed zone — so that
	// home is not in here. Its root is: a command that does not care where it runs
	// starts there rather than failing to start at all.
	cwd := m.Cwd
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(s.p.Pid), "root", cwd)); err != nil {
		cwd = "/"
	}
	req := &proto.Msg{Op: proto.OpSpawn, Argv: m.Argv, Env: m.Env, Cwd: cwd,
		TTY: m.TTY, Session: m.Session}
	var reply rpcReply
	var err error
	if m.TTY {
		req.AllocPTY, req.Rows, req.Cols = true, m.Rows, m.Cols
		reply, err = s.callFD(req)
	} else {
		reply, err = s.callFD(req, fds[0], fds[1], fds[2])
	}
	if err != nil {
		return 0, err
	}
	pid := reply.msg.Pid
	s.mu.Lock()
	s.waits[pid] = wait
	s.mu.Unlock()

	if m.TTY {
		if len(reply.fds) != 1 {
			closeAll(reply.fds)
			return 0, fmt.Errorf("the node pod did not return a terminal")
		}
		master := reply.fds[0]
		// Hand the terminal to the caller and let it relay: it is the one holding
		// the other pty, and it is the one that hears about a window resize.
		if err := c.Send(&proto.Msg{Op: proto.OpSpawned, ID: m.ID, Pid: pid},
			master); err != nil {
			syscall.Close(master)
			return 0, err
		}
		syscall.Close(master)
		return <-wait, nil
	}
	closeAll(reply.fds)
	return <-wait, nil
}

// readLoop demultiplexes vpinit's replies, exactly as the daemon's does.
func (s *server) readLoop() {
	for {
		m, fds, err := s.p.Conn.Recv()
		if err != nil {
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

func (s *server) call(m *proto.Msg, fds ...int) (*proto.Msg, error) {
	r, err := s.callFD(m, fds...)
	if err != nil {
		return nil, err
	}
	closeAll(r.fds)
	return r.msg, nil
}

func (s *server) callFD(m *proto.Msg, fds ...int) (rpcReply, error) {
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
	case <-time.After(30 * time.Second):
		// Everything asked of vpinit is a syscall away, so a reply this late means
		// something is wrong rather than busy. Nobody is watching a node, so
		// silence here would be a hang with no symptom at all.
		s.rpcMu.Lock()
		delete(s.pending, id)
		s.rpcMu.Unlock()
		return rpcReply{}, fmt.Errorf("the node pod did not answer %q in 30s", m.Op)
	}
}

// Version is stamped into the binary so a node running a pushed copy from an
// older build is noticed rather than debugged.
var Version = "dev"

func closeAll(fds []int) {
	for _, fd := range fds {
		syscall.Close(fd)
	}
}

// Exec is `vibepod nodeexec`: the other half of a dispatch.
//
// It runs on the node, reached by one ssh, and hands its own descriptors to the
// node pod through the socket. The daemon is therefore never in the data path,
// and the command's stdout and stderr stay separate all the way back — which
// agents need, and which a PTY would have merged.
func Exec(args []string) error {
	podName, tty := "", false
	var argv []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pod":
			if i+1 < len(args) {
				i++
				podName = args[i]
			}
		case "--tty":
			tty = true
		case "--":
			argv = args[i+1:]
			i = len(args)
		}
	}
	if podName == "" {
		return fmt.Errorf("nodeexec needs --pod")
	}
	if len(argv) == 0 {
		return fmt.Errorf("nodeexec needs a command after --")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	c, err := proto.Dial(SockPath(home, podName))
	if err != nil {
		return fmt.Errorf("no node pod %q on this machine: %w", podName, err)
	}
	defer c.Close()
	cwd, _ := os.Getwd()
	req := &proto.Msg{Op: proto.OpSpawn, Argv: argv, Env: os.Environ(), Cwd: cwd,
		TTY: tty}
	if tty {
		req.Rows, req.Cols, _ = sys.GetWinsize(os.Stdin.Fd())
	}
	if err := c.Send(req, 0, 1, 2); err != nil {
		return err
	}
	for {
		reply, fds, err := c.Recv()
		if err != nil {
			closeAll(fds)
			return err
		}
		switch reply.Op {
		case proto.OpSpawned:
			if len(fds) != 1 {
				closeAll(fds)
				return fmt.Errorf("the node pod did not return a terminal")
			}
			relay(os.NewFile(uintptr(fds[0]), "pod-tty"))
		case proto.OpExit:
			os.Exit(reply.Code)
		case proto.OpErr:
			closeAll(fds)
			return fmt.Errorf("%s", reply.Err)
		default:
			closeAll(fds)
			return fmt.Errorf("unexpected reply %q", reply.Op)
		}
	}
}

// relay joins the terminal ssh gave us to the one the pod gave us.
//
// Raw on this side, because the pod's terminal is doing the line editing and echo;
// doing it twice is the classic doubled-characters mess. Window changes are passed
// on, since a program on the far side that redraws needs to know.
func relay(master *os.File) {
	state, err := term.MakeRaw(os.Stdin.Fd())
	if err == nil {
		defer state.Restore()
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if r, c, err := sys.GetWinsize(os.Stdin.Fd()); err == nil {
				_ = sys.SetWinsize(master.Fd(), r, c)
			}
		}
	}()
	go func() { _, _ = io.Copy(master, os.Stdin) }()
	_, _ = io.Copy(os.Stdout, master)
	signal.Stop(winch)
}

// Down stops a node pod and releases its mounts. It is `vibepod nodedown`, run
// over ssh, and it is what `vibepod down` reaches for each node.
func Down(args []string) error {
	podName := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--pod" && i+1 < len(args) {
			i++
			podName = args[i]
		}
	}
	if podName == "" {
		return fmt.Errorf("nodedown needs --pod")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sock := SockPath(home, podName)
	c, err := proto.Dial(sock)
	if err != nil {
		// Nothing listening: either it is already gone, or it died and left its
		// mounts. Clear them either way — a mountpoint whose server is gone fails
		// every access until it is detached.
		fs.UnmountUnder(RunDir(home, podName))
		_ = os.RemoveAll(RunDir(home, podName))
		return nil
	}
	// Closing the socket is the shutdown: the accept loop returns, which kills the
	// pod and releases the mounts on its way out.
	_ = c.Send(&proto.Msg{Op: proto.OpDown})
	_, _, _ = c.Recv()
	c.Close()
	// Then the state goes, because the promise is that nothing is left on a machine
	// vibepod was asked to leave. The binary in ~/.vp/bin stays — it is the
	// footprint that was consented to, and re-pushing it on the next `up` would be
	// a minute of somebody's time for nothing — so `rm -rf ~/.vp` remains the way
	// to remove vibepod from a machine entirely.
	run := RunDir(home, podName)
	for i := 0; i < 50; i++ {
		if !alive(run) {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	fs.UnmountUnder(run)
	_ = os.RemoveAll(run)
	return nil
}

// alive reports whether the vpnode named by a run directory's pidfile is still
// running, so cleanup waits for it to release its mounts rather than racing it.
func alive(runDir string) bool {
	b, err := os.ReadFile(filepath.Join(runDir, "vpnode.pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// Ctl is `vibepod nodectl`: one control message in, one reply out, both as JSON.
//
// It exists because a node pod is reached by ssh and nothing else. The daemon needs
// to ask it things — what do you hold, take this mount, release that one, forget
// what you cached, I am still here — and the alternative to a generic pipe is a
// flag per question and a parser to match.
func Ctl(args []string) error {
	podName := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--pod" && i+1 < len(args) {
			i++
			podName = args[i]
		}
	}
	if podName == "" {
		return fmt.Errorf("nodectl needs --pod")
	}
	var m proto.Msg
	if err := json.NewDecoder(os.Stdin).Decode(&m); err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	c, err := proto.Dial(SockPath(home, podName))
	if err != nil {
		return fmt.Errorf("no node pod %q on this machine: %w", podName, err)
	}
	defer c.Close()
	if err := c.Send(&m); err != nil {
		return err
	}
	reply, fds, err := c.Recv()
	closeAll(fds)
	if err != nil {
		return err
	}
	out, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", out)
	if reply.Op == proto.OpErr {
		// The message is in the JSON; the status is for the ssh that carried it.
		os.Exit(1)
	}
	return nil
}
