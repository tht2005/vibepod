package daemon

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/route"
)

// servePodSocket answers the socket that is bound into the pod. Everything
// arriving here comes from inside the namespace — vpsh recording a command line,
// a /vp/bin wrapper dispatching one, or an agent calling `vp` — so it is
// deliberately the weaker of the daemon's two sockets. Which ops it accepts is
// podOp(), in daemon.go.
func (d *Daemon) servePodSocket(s *podState) {
	for {
		c, err := s.podLn.AcceptUnix()
		if err != nil {
			return
		}
		k := &ctlConn{d: d, c: proto.NewConn(c), host: false, pod: s}
		go k.serve()
	}
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

// recordShellCommand is what the pod's $SHELL reports: a command line, as
// written, about to run in the pod.
//
// This is the whole of what replaced the exec gate's observation half, and the
// trade is stated in DESIGN.md §3. It sees what agents and humans actually do —
// they shell out — and it records the line as typed rather than the resolved
// argv of an execve. It does not see a direct execve from a bundled binary, and
// that is the cost that was bought deliberately.
func (k *ctlConn) recordShellCommand(m *proto.Msg) {
	s := k.pod
	if s == nil {
		return
	}
	pid := k.c.PeerPID()
	session := m.Session
	if id := sessionOf(uint32(pid)); id != "" {
		session = id
	}
	argv := m.Argv
	if len(argv) == 0 {
		return
	}
	rec := &execRec{PID: pid, PPID: parentOf(pid), Argv: argv, Cwd: m.Cwd,
		Target: route.Pod, Session: session, Start: time.Now()}
	s.recordExec(rec)

	// A session that has moved to another machine still runs its *shell* here:
	// an agent's wrapper sources a snapshot from this machine and writes its new
	// cwd to a temp file on this machine, so shipping the wrapper would break
	// both, silently (§3). Say so once per session rather than per command —
	// once is information, every time is noise — and say it where the routing
	// decisions go, not on the command's stderr, which agents parse.
	backend := s.backendOf(session)
	if backend == route.Pod || session == "" {
		return
	}
	s.mu.Lock()
	told := s.toldLocal[session]
	s.toldLocal[session] = true
	s.mu.Unlock()
	if told {
		return
	}
	s.d.bus.Publish(event.Event{Kind: event.KindNotice, Pod: s.name,
		Target: route.Pod, Argv: argv, Session: session,
		Detail: fmt.Sprintf("this session is on %s, but a shell runs in the pod; "+
			"use `vp @%s …` for one command", backend, backend)})
}

// sessionOf reads the session token out of a pod process's environment. It is
// inherited, so every descendant of a session answers with that session — and it
// is read from /proc rather than taken from the message, so a process inside the
// pod cannot claim to be a session it is not.
func sessionOf(pid uint32) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return ""
	}
	for _, kv := range bytes.Split(b, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("VIBEPOD_SESSION=")); ok {
			return string(v)
		}
	}
	return ""
}

// hasTTY reports whether a pod process has a terminal on stdin.
func hasTTY(pid int) bool {
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", pid))
	if err != nil {
		return false
	}
	return strings.HasPrefix(link, "/dev/pts/")
}
