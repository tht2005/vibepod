package daemon

import (
	"fmt"
	"sort"

	"vibepod/internal/event"
	"vibepod/internal/proto"
)

// Port forwards.
//
// A notebook server on gpu03, a dev server on aiotlab: `ports: [gpu03:8888]`
// makes each reachable here as localhost, on the multiplexed connection vibepod
// already holds. The pod shares this machine's network, so the agent reaches them
// the same way — and a forward goes away with the pod rather than outliving it as
// a stray `ssh -L` in some terminal.

// forward opens one port and remembers it, so `down` can close it and `vp forward`
// can list it.
func (s *podState) forward(p proto.PortSpec, pr *Progress) error {
	if !s.knownBackend(p.Host) {
		return fmt.Errorf("this pod has no machine called %q; `vp hosts` lists them", p.Host)
	}
	s.mu.Lock()
	for _, have := range s.ports {
		if have.Local == p.Local {
			s.mu.Unlock()
			if have == p {
				return nil
			}
			return fmt.Errorf("local port %d already forwards to %s:%d", p.Local,
				have.Host, have.Remote)
		}
	}
	s.mu.Unlock()
	h := s.d.pool.Host(p.Host)
	pr.step("forwarding localhost:%d to %s:%d… ", p.Local, p.Host, p.Remote)
	if err := h.Warm(); err != nil {
		pr.failed()
		return err
	}
	if err := h.Forward(p.Local, p.Remote); err != nil {
		pr.failed()
		return err
	}
	pr.ok("open")
	s.markUsed(p.Host)
	s.mu.Lock()
	s.ports = append(s.ports, p)
	s.mu.Unlock()
	s.d.bus.Publish(event.Event{Kind: event.KindPod, Pod: s.name, Target: p.Host,
		Detail: fmt.Sprintf("forwarding localhost:%d to %s:%d", p.Local, p.Host, p.Remote)})
	return nil
}

func (s *podState) portList() []proto.PortSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]proto.PortSpec(nil), s.ports...)
	sort.Slice(out, func(i, j int) bool { return out[i].Local < out[j].Local })
	return out
}

func (s *podState) closePorts() {
	for _, p := range s.portList() {
		s.d.pool.Host(p.Host).Unforward(p.Local, p.Remote)
	}
}
