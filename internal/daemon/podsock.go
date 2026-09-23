package daemon

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"

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
		go (&podConn{d: d, s: s, c: proto.NewConn(c)}).serve()
	}
}

// podConn is one vpsh (or in-pod vpctl) connection. It keeps reading while a
// command runs, because a Ctrl-C arrives on this same connection and must not
// wait for the command it is meant to interrupt.
type podConn struct {
	d *Daemon
	s *podState
	c *proto.Conn

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

	if dec.Target == route.Pod {
		stash, ok := s.stashOf(m.Path)
		if !ok {
			// The shim is only reachable because it was bound over this path,
			// so the stash must exist. If it does not, something unmounted it.
			pc.send(&proto.Msg{Op: proto.OpErr, ID: m.ID,
				Err: "no stashed original for " + m.Path})
			return
		}
		pc.send(&proto.Msg{Op: proto.OpRunLocal, ID: m.ID, Path: stash,
			Target: dec.Target})
		return
	}
	code, err := pc.runRemote(dec, m, files)
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
