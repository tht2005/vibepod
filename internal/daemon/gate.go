package daemon

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vibepod/internal/pod"
	"vibepod/internal/route"
	"vibepod/internal/sys"
)

// execveat, when the pathname is empty and AT_EMPTY_PATH is set, runs the
// dirfd itself.
const (
	nrExecve     = 59
	nrExecveat   = 322
	atEmptyPath  = 0x1000
	maxPathBytes = 4096
)

// shellNames never get a shim. An agent wraps its commands in a generated
// shell script that sources a local environment snapshot and writes the new
// cwd to a local temp file; shipping that string to another machine breaks
// both. Interception belongs one level down, at the program the shell runs.
var shellNames = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"fish": true, "busybox": true,
}

// runGate reads the pod's exec notifications forever. This loop is the single
// point where vibepod learns that a program is about to start, and it must
// answer every notification: an unanswered one leaves that process frozen.
func (d *Daemon) runGate(s *podState) {
	for {
		n, err := sys.NotifRecv(s.p.GateFD)
		if err != nil {
			d.logf("pod %s: exec gate closed: %v", s.name, err)
			return
		}
		go d.handleExec(s, n)
	}
}

func (d *Daemon) handleExec(s *podState, n *sys.Notif) {
	// Whatever happens below, let the exec proceed. A failure here costs a
	// command its routing — it runs in the pod, against the mount, which is
	// slower but still correct. Freezing the agent instead would not be.
	defer func() {
		if err := sys.NotifContinue(s.p.GateFD, n.ID); err != nil {
			d.logf("pod %s: reply to exec notification: %v", s.name, err)
		}
	}()

	path, err := execPath(n)
	if err != nil {
		return
	}
	cwd, err := sys.Cwd(n.PID)
	if err != nil {
		return
	}
	// The process may have died and its pid been reused between the
	// notification and these reads; if so, everything above is about some
	// other process and must not be acted on.
	if !sys.NotifIDValid(s.p.GateFD, n.ID) {
		return
	}
	if !d.shouldShim(s, path, cwd, n.PID) {
		return
	}
	if _, err := s.ensureShim(path); err != nil {
		d.logf("pod %s: %v", s.name, err)
	}
}

// shouldShim decides whether this binary needs to be redirected. It is
// deliberately generous: the shim re-decides with full information, so a shim
// that turns out to run locally costs one round trip, while a missing shim
// costs a command that ran on the wrong machine.
func (d *Daemon) shouldShim(s *podState, path, cwd string, pid uint32) bool {
	if strings.HasPrefix(path, pod.VpDir+"/") {
		return false // the pod's own machinery, including the stash
	}
	if shellNames[filepath.Base(path)] {
		return false
	}
	if s.shimAll {
		return true
	}
	if route.Route(s.table, cwd, "") != route.Pod {
		return true
	}
	// A session pin can send a command somewhere the directory never would.
	if pin := s.pinOf(sessionOf(pid)); pin != "" && pin != route.Pod {
		return true
	}
	return false
}

// execPath recovers the program a frozen process is about to run. The
// notification carries only a pointer into that process's address space.
func execPath(n *sys.Notif) (string, error) {
	switch n.NR {
	case nrExecve:
		return sys.ReadCString(n.PID, n.Args[0], maxPathBytes)
	case nrExecveat:
		p, err := sys.ReadCString(n.PID, n.Args[1], maxPathBytes)
		if err != nil {
			return "", err
		}
		if p == "" && n.Args[4]&atEmptyPath != 0 {
			// Executing an already-open fd, as a bundled runtime may do.
			return os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", n.PID, int32(n.Args[0])))
		}
		return p, nil
	}
	return "", fmt.Errorf("unexpected syscall %d", n.NR)
}

// sessionOf reads the session token out of a pod process's environment. It is
// inherited, so every descendant of a session answers with that session.
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
