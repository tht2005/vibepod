package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/route"
)

// streamLog replays what has happened and, if asked, keeps going.
//
// This is the trust surface. For a tool whose pitch is "your agent runs
// commands on prod", a provable history of what ran and where is not a
// debugging afterthought.
func (d *Daemon) streamLog(c *proto.Conn, m *proto.Msg) {
	send := func(e event.Event) bool {
		if m.Pod != "" && e.Pod != m.Pod {
			return true
		}
		b, err := json.Marshal(e)
		if err != nil {
			return true
		}
		return c.Send(&proto.Msg{Op: proto.OpEvent, ID: m.ID, Event: b}) == nil
	}
	var ch <-chan event.Event
	var stop func()
	if m.Follow {
		// Subscribe before replaying history, so nothing falls between the two.
		ch, stop = d.bus.Subscribe()
		defer stop()
	}
	seen := map[string]bool{}
	for _, e := range d.bus.History() {
		seen[e.Time] = true
		if !send(e) {
			return
		}
	}
	if !m.Follow {
		_ = c.Send(&proto.Msg{Op: proto.OpEnd, ID: m.ID})
		return
	}
	for e := range ch {
		if seen[e.Time] {
			continue
		}
		if !send(e) {
			return
		}
	}
}

func (d *Daemon) treeOf(m *proto.Msg) (*proto.Tree, error) {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return nil, err
	}
	t := s.tree(m.All)
	if m.Detail == "expand" {
		s.expandRemote(t)
	}
	return t, nil
}

// expandRemote is `vp tree -x`: for each command still running on another
// machine, ask that machine what is running under it. vibepod knows what it
// dispatched and not what that spawned, so this is a poll, it is marked as one,
// and it only happens when asked.
func (s *podState) expandRemote(t *proto.Tree) {
	var wg sync.WaitGroup
	var walk func(n *proto.TreeNode)
	walk = func(n *proto.TreeNode) {
		if n.State == "running" && n.ExecID != "" && n.Target != route.Pod {
			wg.Add(1)
			go func(n *proto.TreeNode) {
				defer wg.Done()
				lines, err := s.d.pool.Host(n.Target).Children(n.ExecID)
				if err != nil {
					return
				}
				for _, l := range lines {
					pid, args, _ := strings.Cut(l, " ")
					var p int
					fmt.Sscan(pid, &p)
					n.Remote = append(n.Remote, proto.RemoteProc{PID: p,
						Args: strings.TrimSpace(args)})
				}
			}(n)
		}
		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	for i := range t.Sessions {
		for j := range t.Sessions[i].Nodes {
			walk(&t.Sessions[i].Nodes[j])
		}
	}
	wg.Wait()
}
