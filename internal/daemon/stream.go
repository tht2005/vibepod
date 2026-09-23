package daemon

import (
	"encoding/json"

	"vibepod/internal/event"
	"vibepod/internal/proto"
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
	return s.tree(m.All), nil
}
