package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/route"
)

// The control plane: keeping N pods holding the same tree.
//
// The composed tree changes while the pod runs (§9), so it is replicated mutable
// state, and the shape that works for that is a reconciler rather than a
// transaction:
//
//   - the desired state is the ordered mount list, each entry with a monotonic
//     generation
//   - each node pod reports what it holds
//   - a change bumps the generation and pushes; each node converges on its own; one
//     that was unreachable applies the difference when it comes back
//   - **dispatch is the guard**: a command whose directory resolves into a mount
//     the target does not hold is refused, naming both sides
//
// Two-phase commit is the obvious alternative and is rejected: one unreachable node
// would block every mount change, and it blocks on participants that may never
// return. Best-effort push with no tracking is rejected for the opposite reason —
// it produces exactly the silent divergence this exists to prevent.
//
// Staleness is tracked per **mount**, not per node. If one machine missed a new
// mount, commands there under other mounts are still correct and keep working;
// refusal is scoped to the paths actually affected. That is the difference between
// one missed mount costing a path and costing a machine.

// generation is the version of the desired mount list. It only goes up.
func (s *podState) generation() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

func (s *podState) bumpGeneration() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	return s.gen
}

// desired is the mount list a node pod should hold: the composed zone, in order,
// minus the identity plane, and minus this machine's own directories unless the
// config exposed them to that node — sending local files to a remote machine is a
// decision about where data goes, so it is never the default.
//
// Via is decided here, per node: direct, or relayed through this machine.
func (s *podState) desired(host string) []proto.MountSpec {
	var out []proto.MountSpec
	for _, m := range s.mountList() {
		if m.Identity {
			continue
		}
		if m.Owner == route.Pod && !contains(m.ExposeTo, host) {
			continue
		}
		spec := proto.MountSpec{At: m.At, Host: m.Owner, Path: m.RemotePath,
			ReadOnly: m.ReadOnly || !s.isWriter(host, m.At), Cache: m.Cache,
			Prefetch: m.Prefetch, Generation: m.Gen, Via: s.via(host, m)}
		if m.Owner == route.Pod {
			spec.Host, spec.Path = "", m.Src
		}
		out = append(out, spec)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (s *podState) mountAt(at string) *mountRec {
	for _, m := range s.mountList() {
		if m.At == at {
			return m
		}
	}
	return nil
}

// isWriter reports whether this machine is the one allowed to write to a mount.
//
// One write-back cache per mount, and this is where that is decided. §6a asked for
// per-file write tokens; there is no enforcement point for them — the writes go
// through rclone on the node, and vibepod is deliberately not in the data path, so
// it cannot see an open. What it *can* do is decide at mount time, which is
// enforced by the kernel rather than by bookkeeping: the first node pod to take a
// writable mount keeps it, and the others get it read-only and are told so.
//
// The cost is real and is the reason this is not the design's answer: two machines
// cannot write different files in one mount. When that is what you want, and you
// accept that the last flush of any *same* file wins, `writers: many` says so.
func (s *podState) isWriter(host, at string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manyWriters[at] {
		return true
	}
	if who, taken := s.writers[at]; taken {
		return who == host
	}
	if s.writers == nil {
		s.writers = map[string]string{}
	}
	s.writers[at] = host
	return true
}

// releaseWriter gives up a machine's claim on writing to a mount, so the next one
// can have it. Called when a node pod goes away.
func (s *podState) releaseWriter(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for at, who := range s.writers {
		if who == host {
			delete(s.writers, at)
		}
	}
}

// reconcile brings one node pod to the current desired state and reports what it
// now holds. A node that cannot be reached is left behind rather than blocking:
// that is the whole point of the shape.
func (s *podState) reconcile(np *nodePod) error {
	want := s.desired(np.host)
	held, err := s.nodeHeld(np)
	if err != nil {
		np.setReachable(false)
		return err
	}
	byPath := map[string]proto.Held{}
	for _, h := range held {
		byPath[h.At] = h
	}
	wantPath := map[string]bool{}

	for _, m := range want {
		wantPath[m.At] = true
		h, have := byPath[m.At]
		// Identity, not just the path: a path can be unmounted and remounted from
		// a different machine, and a node still holding the old one is the case
		// generations exist to catch.
		if have && h.Host == m.Host && h.Path == m.Path && h.ReadOnly == m.ReadOnly {
			continue
		}
		if have {
			if err := s.nodeCall(np, &proto.Msg{Op: proto.OpUnmount, Path: m.At}); err != nil {
				return fmt.Errorf("%s: release the old %s: %w", np.host, m.At, err)
			}
		}
		if m.Via == "relay" {
			rec := s.mountAt(m.At)
			if rec == nil {
				continue
			}
			relay, err := s.relayFor(np, rec)
			if err != nil {
				return fmt.Errorf("%s: relay %s: %w", np.host, m.At, err)
			}
			m.Relay = relay
		}
		if err := s.nodeCall(np, &proto.Msg{Op: proto.OpMount,
			Mounts: []proto.MountSpec{m}}); err != nil {
			return fmt.Errorf("%s: %w", np.host, err)
		}
		s.d.logf("pod %s: %s took %s at generation %d", s.name, np.host, m.At, m.Generation)
	}
	// Anything it holds that the list no longer names. Flushed first by the node,
	// which refuses rather than discarding unwritten data.
	for _, h := range held {
		if wantPath[h.At] {
			continue
		}
		if err := s.nodeCall(np, &proto.Msg{Op: proto.OpUnmount, Path: h.At}); err != nil {
			return fmt.Errorf("%s: release %s: %w", np.host, h.At, err)
		}
		s.d.logf("pod %s: %s released %s", s.name, np.host, h.At)
	}

	final, err := s.nodeHeld(np)
	if err != nil {
		np.setReachable(false)
		return err
	}
	np.setHeld(final, s.generation())
	return nil
}

// reconcileAll converges every node pod. Called after any change to the mount list,
// and again when a machine that was unreachable answers.
func (s *podState) reconcileAll(why string) {
	for _, np := range s.nodePods.list() {
		if err := s.reconcile(np); err != nil {
			s.d.logf("pod %s: %s is behind after %s: %v", s.name, np.host, why, err)
			s.d.bus.Publish(event.Event{Kind: event.KindNotice, Pod: s.name,
				Target: np.host, Detail: "behind: " + err.Error()})
			continue
		}
		s.d.logf("pod %s: %s is at generation %d", s.name, np.host, s.generation())
	}
}

// nodeHeld asks a machine what its pod holds.
func (s *podState) nodeHeld(np *nodePod) ([]proto.Held, error) {
	reply, err := s.nodeRequest(np, &proto.Msg{Op: proto.OpHeld})
	if err != nil {
		return nil, err
	}
	return reply.Held, nil
}

func (s *podState) nodeCall(np *nodePod, m *proto.Msg) error {
	_, err := s.nodeRequest(np, m)
	return err
}

// nodeRequest sends one control message to a node pod over ssh.
//
// One ssh per request, on the multiplexed connection, which costs a round trip.
// That is the right trade for control traffic: it is rare, and the alternative is a
// long-lived channel to keep alive and reason about on top of the lease that
// already exists for exactly that purpose.
func (s *podState) nodeRequest(np *nodePod, m *proto.Msg) (*proto.Msg, error) {
	blob, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	h := s.d.pool.Host(np.host)
	out, err := h.Feed(fmt.Sprintf("%s/.vp/bin/vibepod nodectl --pod %s", np.home,
		s.nodeName(np.host)), blob)
	if err != nil {
		if line := lastJSONError(out); line != "" {
			return nil, fmt.Errorf("%s", line)
		}
		return nil, err
	}
	var reply proto.Msg
	if err := json.Unmarshal([]byte(strings.TrimSpace(lastLine(out))), &reply); err != nil {
		return nil, fmt.Errorf("%s answered %q", np.host, strings.TrimSpace(out))
	}
	if reply.Op == proto.OpErr {
		return nil, fmt.Errorf("%s", reply.Err)
	}
	np.setReachable(true)
	return &reply, nil
}

// lastLine is the reply: ssh may have printed a banner or a warning first.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// lastJSONError pulls the explanation out of a failed nodectl, which prints its
// reply and then exits non-zero so ssh reports the failure too.
func lastJSONError(out string) string {
	var reply proto.Msg
	if json.Unmarshal([]byte(strings.TrimSpace(lastLine(out))), &reply) != nil {
		return ""
	}
	return reply.Err
}

// leaseLoop keeps every node pod alive, and is the only thing that does.
//
// A node pod takes itself down when it has not heard from the daemon for its lease.
// That is deliberate: dying with the ssh channel would kill an eight-hour run
// because a laptop lid closed, and living forever would leave caches and a
// namespace on a machine other people share. So the daemon says "still here" while
// it is, and stops saying it when it is gone.
//
// It is also when a machine that was unreachable gets noticed: the renewal fails,
// then succeeds, and the reconciler runs.
func (s *podState) leaseLoop() {
	tick := time.NewTicker(s.renewEvery())
	defer tick.Stop()
	for {
		select {
		case <-s.stopped:
			return
		case <-tick.C:
		}
		for _, np := range s.nodePods.list() {
			wasReachable := np.isReachable()
			if err := s.nodeCall(np, &proto.Msg{Op: proto.OpLease}); err != nil {
				if wasReachable {
					s.d.logf("pod %s: %s did not answer: %v", s.name, np.host, err)
					np.setReachable(false)
				}
				continue
			}
			if !wasReachable {
				// It is back. Whatever changed while it was away is the difference
				// the reconciler exists to apply.
				s.d.logf("pod %s: %s is reachable again; reconciling", s.name, np.host)
				if err := s.reconcile(np); err != nil {
					s.d.logf("pod %s: %s: %v", s.name, np.host, err)
				}
			}
		}
	}
}

// renewEvery is how often the daemon says it is still here: a third of the lease,
// so one failed renewal is not an expired lease, and never more than thirty
// seconds, so a machine coming back is noticed without much delay.
func (s *podState) renewEvery() time.Duration {
	every := 30 * time.Second
	if d, err := time.ParseDuration(s.lease); err == nil && d > 0 && d/3 < every {
		every = d / 3
	}
	if every < 100*time.Millisecond {
		every = 100 * time.Millisecond
	}
	return every
}

// staleness is the per-mount answer for `vp ps`, the cockpit and a refused
// dispatch: which machines are behind, and on what.
func (s *podState) staleness() map[string][]string {
	out := map[string][]string{}
	for _, np := range s.nodePods.list() {
		want := map[string]bool{}
		for _, m := range s.desired(np.host) {
			want[m.At] = true
		}
		have := map[string]bool{}
		for _, h := range np.heldList() {
			have[h.At] = true
		}
		var behind []string
		for at := range want {
			if !have[at] {
				behind = append(behind, at)
			}
		}
		sort.Strings(behind)
		if len(behind) > 0 {
			out[np.host] = behind
		}
	}
	return out
}

// writerFor reports which machine may write to a mount, for the views. Empty means
// nobody has claimed it, which is the ordinary state until a node pod takes one.
func (s *podState) writerFor(at string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writers[at]
}

// fanOutInvalidate tells every node pod except the one a command ran on to forget
// what it cached for the mount that command was in. Per mount: a command in one
// directory is no reason to throw away another's cache.
//
// Asynchronous, and best effort. The command has already finished and its caller
// is waiting for the exit code; a node that is slow to hear about it is exactly as
// stale as it was a moment ago, and the backend's own timeouts bound that.
func (s *podState) fanOutInvalidate(ranOn, cwd string) {
	at := ""
	for _, r := range s.table().Rules() {
		if under(cwd, r.Prefix) && r.Owner != route.Pod {
			at = r.Prefix
			break
		}
	}
	if at == "" {
		return
	}
	for _, np := range s.nodePods.list() {
		if np.host == ranOn || !np.holds(at) {
			continue
		}
		go func(np *nodePod) {
			if err := s.nodeCall(np, &proto.Msg{Op: proto.OpInvalidate, Path: at}); err != nil {
				s.d.logf("pod %s: tell %s to forget %s: %v", s.name, np.host, at, err)
			}
		}(np)
	}
}

var _ = sync.Mutex{}

// sshdMaxSessions is OpenSSH's default MaxSessions. A machine may raise it; vibepod
// cannot ask sshd what it is set to without privileges, so this is the number the
// warning is measured against, and the warning says so.
const sshdMaxSessions = 10

// connectionBudget reports, per machine that owns mounts, how many sftp sessions
// this pod's caches open against it: one per mount here, plus one per mount per
// node pod that caches it rather than owning it.
//
// It exists because the failure it predicts does not look like itself. Past sshd's
// MaxSessions the next session is refused, and on a shared login node that is
// somebody else's `git pull` failing for no reason they can see.
func (s *podState) connectionBudget() map[string]int {
	perOwner := map[string]int{}
	for _, m := range s.mountList() {
		if m.Identity || m.Owner == route.Pod {
			continue
		}
		perOwner[m.Owner]++ // this machine's own mount of it
		for _, np := range s.nodePods.list() {
			if np.host == m.Owner {
				continue // a native bind costs no session
			}
			for _, h := range np.heldList() {
				if h.At == m.At && !h.Native {
					perOwner[m.Owner]++
				}
			}
		}
	}
	return perOwner
}

// warnBudget says so when a machine is close to refusing sessions.
func (s *podState) warnBudget(pr *Progress) {
	for owner, n := range s.connectionBudget() {
		if n < sshdMaxSessions-2 {
			continue
		}
		msg := fmt.Sprintf("%s: %d sftp sessions from this pod's caches, against "+
			"sshd's default MaxSessions of %d — past it, the next session anyone "+
			"opens there is refused", owner, n, sshdMaxSessions)
		s.d.logf("pod %s: %s", s.name, msg)
		s.d.bus.Publish(event.Event{Kind: event.KindNotice, Pod: s.name,
			Target: owner, Detail: msg})
		if pr != nil {
			pr.step("%s\n", msg)
		}
	}
}
