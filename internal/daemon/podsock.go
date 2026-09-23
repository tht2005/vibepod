package daemon

import (
	"fmt"

	"vibepod/internal/proto"
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
		go d.servePodConn(s, proto.NewConn(c))
	}
}

func (d *Daemon) servePodConn(s *podState, c *proto.Conn) {
	defer c.Close()
	for {
		m, fds, err := c.Recv()
		if err != nil {
			closeAll(fds)
			return
		}
		switch m.Op {
		case proto.OpExec:
			d.routeExec(s, c, m, fds)
			closeAll(fds)
		case proto.OpPs:
			_ = c.Send(&proto.Msg{Op: proto.OpOK, ID: m.ID, Pods: d.ps()})
		default:
			closeAll(fds)
			_ = c.Errorf(m.ID, "%q is not permitted from inside a pod", m.Op)
		}
	}
}

// routeExec answers the one question vpsh exists to ask: this command, from
// this directory — where does it run?
func (d *Daemon) routeExec(s *podState, c *proto.Conn, m *proto.Msg, fds []int) {
	target := route.Route(s.table, m.Cwd, s.pinOf(m.Session))
	d.logf("pod %s: exec %v cwd=%s -> %s", s.name, m.Argv, m.Cwd, target)

	if target == route.Pod {
		stash, ok := s.stashOf(m.Path)
		if !ok {
			// The shim is reachable only because it was bound over this path,
			// so the stash must exist. If it does not, something unmounted it.
			_ = c.Errorf(m.ID, "no stashed original for %s", m.Path)
			return
		}
		_ = c.Send(&proto.Msg{Op: proto.OpRunLocal, ID: m.ID, Path: stash,
			Target: target})
		return
	}
	code, err := d.runRemote(s, target, m, fds)
	if err != nil {
		_ = c.Errorf(m.ID, "%v", err)
		return
	}
	_ = c.Send(&proto.Msg{Op: proto.OpExit, ID: m.ID, Code: code, Target: target})
}

// runRemote is filled in by M1.
func (d *Daemon) runRemote(s *podState, target string, m *proto.Msg, fds []int) (int, error) {
	return 0, fmt.Errorf("routing to %q needs a remote, which this build does not have yet", target)
}
