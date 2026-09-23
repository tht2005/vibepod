package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"vibepod/internal/pod"
	"vibepod/internal/proto"
	"vibepod/internal/remote"
	"vibepod/internal/route"
)

// servePodSocket answers the socket that is bound into the pod. Everything
// arriving here comes from inside the namespace — vpsh asking where a command
// should run, or an agent calling vpctl — so it is deliberately the weaker of
// the daemon's two sockets.
func (d *Daemon) servePodSocket(s *podState) {
	for {
		c, err := s.podLn.AcceptUnix()
		if err != nil {
			return
		}
		pc := &podConn{d: d, s: s, c: proto.NewConn(c)}
		pc.pid = pc.c.PeerPID()
		go pc.serve()
	}
}

// podConn is one vpsh (or in-pod vpctl) connection. It keeps reading while a
// command runs, because a Ctrl-C arrives on this same connection and must not
// wait for the command it is meant to interrupt.
type podConn struct {
	d *Daemon
	s *podState
	c *proto.Conn

	pid int // the shim's pid, as the daemon's namespace sees it

	mu      sync.Mutex
	host    *remote.Host
	execID  string
	sending sync.Mutex
}

var execSeq atomic.Uint64

// adopt takes ownership of descriptors received over a socket.
func adopt(fds []int) []*os.File {
	files := make([]*os.File, len(fds))
	for i, fd := range fds {
		files[i] = os.NewFile(uintptr(fd), fmt.Sprintf("fd%d", i))
	}
	return files
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

func (pc *podConn) serve() {
	defer pc.c.Close()
	for {
		m, fds, err := pc.c.Recv()
		if err != nil {
			closeAll(fds)
			return
		}
		switch m.Op {
		case proto.OpExec:
			// Wrap the descriptors once, here: an os.File owns its fd and
			// closes it on finalize, so handing the same number to both
			// os/exec and a raw close would shut down a reused fd later.
			files := adopt(fds)
			go func() {
				defer closeFiles(files)
				pc.routeExec(m, files)
			}()
		case proto.OpSignal:
			pc.forwardSignal(m.Sig)
		case proto.OpPs:
			pc.send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Pods: pc.d.ps()})
		case proto.OpTree:
			t, err := pc.d.treeOf(m)
			if err != nil {
				pc.send(&proto.Msg{Op: proto.OpErr, ID: m.ID, Err: err.Error()})
			} else {
				pc.send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Tree: t})
			}
		case proto.OpLog:
			pc.d.streamLog(pc.c, m)
		case proto.OpUse:
			// Own session only, and only from something holding a terminal.
			if !hasTTY(pc.pid) {
				pc.send(&proto.Msg{Op: proto.OpErr, ID: m.ID, Err: "use needs a terminal; " +
					"run it from `vpctl shell`, or set exec_on: in the config"})
				break
			}
			m.Session = sessionOf(uint32(pc.pid))
			if err := pc.d.use(m); err != nil {
				pc.send(&proto.Msg{Op: proto.OpErr, ID: m.ID, Err: err.Error()})
			} else {
				pc.send(&proto.Msg{Op: proto.OpOK, ID: m.ID})
			}
		default:
			closeAll(fds)
			pc.send(&proto.Msg{Op: proto.OpErr, ID: m.ID,
				Err: fmt.Sprintf("%q is not permitted from inside a pod", m.Op)})
		}
	}
}

// send serialises replies, since an exec reply and a signal acknowledgement
// can race on one connection.
func (pc *podConn) send(m *proto.Msg) {
	pc.sending.Lock()
	defer pc.sending.Unlock()
	_ = pc.c.Send(m)
}

// routeExec answers the one question vpsh exists to ask: this command, from
// this directory — where does it run?
func (pc *podConn) routeExec(m *proto.Msg, files []*os.File) {
	s := pc.s
	dec := route.Resolve(s.table, m.Cwd, s.pinOf(m.Session))
	pc.d.logf("pod %s: exec %v cwd=%s -> %s", s.name, m.Argv, m.Cwd, dec.Target)
	// vpsh knows its session, and therefore any pin, so its decision is the
	// authoritative one. Correct the record the gate made a moment ago.
	if pc.pid != 0 {
		s.retarget(pc.pid, dec.Target)
	}

	if dec.Target == route.Pod {
		stash, ok := s.stashOf(m.Path)
		if !ok {
			// A remote-only tool has no original here to fall back to. Say so
			// in those terms: the command is fine, the directory is the
			// problem, and moving is the fix.
			if strings.HasPrefix(m.Path, pod.BinDir+"/") {
				pc.send(&proto.Msg{Op: proto.OpErr, ID: m.ID, Err: fmt.Sprintf(
					"%s exists only on a remote, and %s runs here; "+
						"run it from a directory that belongs to that machine",
					filepath.Base(m.Path), m.Cwd)})
				return
			}
			// Otherwise the shim is only reachable because it was bound over
			// this path, so the stash must exist unless something unmounted it.
			pc.send(&proto.Msg{Op: proto.OpErr, ID: m.ID,
				Err: "no stashed original for " + m.Path})
			return
		}
		pc.send(&proto.Msg{Op: proto.OpRunLocal, ID: m.ID, Path: stash,
			Target: dec.Target})
		return
	}
	code, err := pc.runRemote(dec, m, files)
	if pc.pid != 0 && err == nil {
		s.setExitCode(pc.pid, code)
	}
	if err != nil {
		pc.send(&proto.Msg{Op: proto.OpErr, ID: m.ID, Err: err.Error()})
		return
	}
	pc.send(&proto.Msg{Op: proto.OpExit, ID: m.ID, Code: code, Target: dec.Target})
}

// runRemote hands the caller's own descriptors to ssh, so output streams
// straight back to the terminal that asked for it.
func (pc *podConn) runRemote(dec route.Decision, m *proto.Msg, files []*os.File) (int, error) {
	if len(files) < 3 {
		return 0, fmt.Errorf("a routed command needs stdin, stdout and stderr")
	}
	host := pc.d.pool.Host(dec.Target)
	pc.s.markUsed(dec.Target)

	id := fmt.Sprintf("%s-%d", pc.s.name, execSeq.Add(1))
	pc.mu.Lock()
	pc.host, pc.execID = host, id
	pc.mu.Unlock()

	req := remote.Req{Dir: dec.Dir, Argv: m.Argv, TTY: m.TTY, ID: id}
	copy(req.Files[:], files[:3])
	code, err := host.Run(req)

	pc.mu.Lock()
	pc.host, pc.execID = nil, ""
	pc.mu.Unlock()

	// The command just finished, and nothing else touches that machine's tree
	// through us — so this is the exact moment the cache may be stale, and the
	// only moment it can have become so.
	if pc.s.fs != nil {
		pc.s.fs.InvalidateAfter(dec.Target)
	}
	return code, err
}

// forwardSignal interrupts a running remote command over a second multiplexed
// channel. ssh does not forward signals without a PTY, and a PTY would merge
// stderr into stdout, which agents parse separately.
func (pc *podConn) forwardSignal(sig int) {
	pc.mu.Lock()
	host, id := pc.host, pc.execID
	pc.mu.Unlock()
	if host == nil || id == "" {
		return
	}
	if err := host.Signal(id, sig); err != nil {
		pc.d.logf("pod %s: forward signal %d to %s: %v", pc.s.name, sig, host.Alias, err)
	}
}

// hasTTY reports whether a pod process has a terminal on stdin.
func hasTTY(pid int) bool {
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", pid))
	if err != nil {
		return false
	}
	return strings.HasPrefix(link, "/dev/pts/")
}
