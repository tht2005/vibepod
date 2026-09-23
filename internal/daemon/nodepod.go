package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"vibepod/internal/event"
	"vibepod/internal/node"
	"vibepod/internal/proto"
	"vibepod/internal/remote"
	"vibepod/internal/route"
)

// Node pods: one namespace, replicated on N machines.
//
// A backend with a node pod is a machine where the composed zone exists at the
// same absolute paths as it does here. That is what makes "the data is on gpu03,
// the GPUs are on gpu05" work without translating anything: a path handed from one
// to the other means the same thing, including the ones written inside a YAML file
// or built at run time, which no rewriting scheme can reach.
//
// What is replicated is the composed zone and nothing else. The node's system
// layer stays its own — gpu05's /opt/rocm and /dev/kfd are the reason to dispatch
// there — and the identity plane is not sent at all. There is no field for it in
// the spec.

// nodePod is what the daemon knows about a pod on another machine.
type nodePod struct {
	host string
	sock string // the socket on that machine
	home string

	mu sync.Mutex
	// held is what that node reports it has, so a dispatch into a mount it does
	// not have is refused by name rather than run against the wrong bytes.
	held []proto.Held
	// gen is the generation it has converged to. Behind the pod's own means there
	// is a difference waiting to be applied.
	gen int64
	// reachable is the last thing the lease loop learned. A node that is not
	// reachable is not wrong, only behind.
	reachable bool
}

func (np *nodePod) setHeld(held []proto.Held, gen int64) {
	np.mu.Lock()
	defer np.mu.Unlock()
	np.held, np.gen, np.reachable = held, gen, true
}

func (np *nodePod) heldList() []proto.Held {
	np.mu.Lock()
	defer np.mu.Unlock()
	return append([]proto.Held(nil), np.held...)
}

func (np *nodePod) generation() int64 {
	np.mu.Lock()
	defer np.mu.Unlock()
	return np.gen
}

func (np *nodePod) setReachable(ok bool) {
	np.mu.Lock()
	defer np.mu.Unlock()
	np.reachable = ok
}

func (np *nodePod) isReachable() bool {
	np.mu.Lock()
	defer np.mu.Unlock()
	return np.reachable
}

// nodePods is the set, keyed by machine.
type nodePods struct {
	mu   sync.Mutex
	pods map[string]*nodePod
}

func (n *nodePods) get(host string) *nodePod {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pods[host]
}

func (n *nodePods) put(host string, p *nodePod) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pods == nil {
		n.pods = map[string]*nodePod{}
	}
	n.pods[host] = p
}

func (n *nodePods) list() []*nodePod {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]*nodePod, 0, len(n.pods))
	for _, p := range n.pods {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].host < out[j].host })
	return out
}

func (n *nodePods) drop(host string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.pods, host)
}

// nodeSpecFor builds what a machine needs to construct its own pod.
//
// The composed zone in config order, minus the identity plane, which never
// leaves this machine. A mount the node owns becomes a native bind over there; the
// rest it mounts from whoever owns them.
func (s *podState) nodeSpecFor(host, home string) (*proto.NodeSpec, error) {
	spec := &proto.NodeSpec{
		Pod:      s.nodeName(host),
		Node:     host,
		RunDir:   node.RunDir(home, s.nodeName(host)),
		Hostname: host,
		SSHExtra: s.nodeSSHExtra(),
		Version:  Version,
		Lease:    s.lease,
	}
	// The same list the reconciler converges to, writer election included, so a
	// freshly built pod and a reconciled one cannot disagree.
	//
	// This machine's own directories are not in it. Reaching them from a node means
	// serving them back out — the reverse mount, which is not built — and refusing
	// the whole pod for that would make node pods useless for the ordinary case,
	// where the agent's own code is local. So a node holds everything it can, and a
	// command sent to it from one of those directories runs in its own home, which
	// is said rather than assumed (see dispatch).
	// Relayed mounts need the node's pod to exist before their tunnel does, so the
	// reconciler adds them straight after the pod is built.
	for _, m := range s.desired(host) {
		if m.Via != "relay" {
			spec.Mounts = append(spec.Mounts, m)
		}
	}

	return spec, nil
}

// nodeName is what a node pod is called on the machine that runs it: this pod's
// name *and* the alias it was reached by.
//
// The pod name alone is not enough. Two ssh aliases for one machine — gpu03 and
// gpu03-direct — are common, and keyed by pod name they would share one node pod:
// the second `node add` would "adopt" the first's pod and reconcile it to its own
// idea of who may write. Keyed by alias, each is its own pod, and adoption still
// finds the right one after this daemon restarts.
func (s *podState) nodeName(host string) string { return s.name + "@" + host }

// nodeSSHExtra are the options a node's own outbound ssh needs. In the ordinary
// case none: the node uses its own ssh config and the agent socket forwarded for
// the mount. A test, or a machine whose config lives somewhere unusual, sets one.
func (s *podState) nodeSSHExtra() []string {
	if f := os.Getenv("VIBEPOD_NODE_SSH_CONFIG"); f != "" {
		return []string{"-F", f}
	}
	return nil
}

// startNodePod builds a pod on one machine.
//
// Everything that can be refused is refused before anything is pushed: the
// machine's own capabilities, then what a mount says it requires of whoever runs
// it. `requires:` turns "is this node actually equivalent?" from something you
// find out when a job fails into something `up` answers.
func (s *podState) startNodePod(host string, consented map[string]bool, pr *Progress) error {
	if p := s.nodePods.get(host); p != nil {
		return nil
	}
	h := s.d.pool.Host(host)
	requires := s.requiresFor(host)

	self, err := os.Executable()
	if err != nil {
		return err
	}
	pr.step("checking %s… ", host)
	need, err := h.Probe(self, s.needsCacheOn(host), requires)
	if err != nil {
		pr.failed()
		return err
	}
	if need.Userns {
		pr.failed()
		return fmt.Errorf("%s has unprivileged user namespaces disabled, so it "+
			"cannot hold a pod; ask for `sysctl -w user.max_user_namespaces=N` "+
			"there, or run that session on `pod`", host)
	}
	if len(need.Missing) > 0 {
		pr.failed()
		return fmt.Errorf("%s is missing %s, which a mount in this pod requires; "+
			"either it is the wrong machine for this work, or the `requires:` list "+
			"is wrong", host, strings.Join(need.Missing, ", "))
	}
	pr.ok("ready")

	if need.Binary || need.Rclone {
		if !consented[host] {
			return fmt.Errorf("a pod on %s needs vibepod%s in %s/.vp/bin, and nothing "+
				"else — no credentials, no agent, no installation outside that "+
				"directory.\n  `vibepod up --push` allows it, or answer the question "+
				"when a terminal is attached", host, rcloneAlso(need), need.HomeDir)
		}
		if need.Binary {
			pr.step("copying vibepod to %s:~/.vp/bin… ", host)
			if err := h.Push(self, "vibepod"); err != nil {
				pr.failed()
				return err
			}
			pr.ok("done")
		}
		if need.Rclone {
			path, err := rclonePath()
			if err != nil {
				return err
			}
			pr.step("copying rclone to %s:~/.vp/bin… ", host)
			if err := h.Push(path, "rclone"); err != nil {
				pr.failed()
				return err
			}
			pr.ok("done")
		}
	}

	// A pod may already be running there: this daemon restarted, or the laptop
	// lost its connection and came back inside the lease. Adopt it rather than
	// kill it — the whole reason a node pod outlives a disconnect is so a job on it
	// does not die with the ssh — and let the reconciler apply whatever changed.
	if !need.Binary {
		candidate := &nodePod{host: host, home: need.HomeDir}
		if _, err := s.nodeRequest(candidate, &proto.Msg{Op: proto.OpHeld}); err == nil {
			pr.step("adopting the pod already running on %s… ", host)
			s.nodePods.put(host, candidate)
			if err := s.reconcile(candidate); err != nil {
				pr.failed()
				s.nodePods.drop(host)
				return fmt.Errorf("%s has a pod that could not be reconciled: %w",
					host, err)
			}
			pr.ok("generation %d", candidate.generation())
			s.markUsed(host)
			s.d.bus.Publish(event.Event{Kind: event.KindPod, Pod: s.name,
				Target: host, Detail: "node pod adopted"})
			return nil
		}
	}

	spec, err := s.nodeSpecFor(host, need.HomeDir)
	if err != nil {
		return err
	}
	blob, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	specPath := filepath.Join(spec.RunDir, "spec.json")
	if err := h.WriteFile(specPath, blob); err != nil {
		return err
	}
	pr.step("building a pod on %s… ", host)
	out, err := h.Capture(fmt.Sprintf("%s/.vp/bin/vibepod vpnode --spec %s",
		need.HomeDir, specPath))
	if err != nil {
		pr.failed()
		return fmt.Errorf("%w", err)
	}
	sock := ""
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ready "); ok {
			sock = rest
		}
	}
	if sock == "" {
		pr.failed()
		return fmt.Errorf("%s did not report a node pod: %s", host,
			strings.TrimSpace(out))
	}
	pr.ok("ready")

	np := &nodePod{host: host, sock: sock, home: need.HomeDir, reachable: true}
	s.nodePods.put(host, np)
	defer s.warnBudget(pr)
	// Ask it what it ended up with rather than assuming the spec was applied
	// whole: adoption of a pod that was already running goes through the same
	// path, and there the answer is genuinely unknown.
	if err := s.reconcile(np); err != nil {
		s.nodePods.drop(host)
		return fmt.Errorf("%s built a pod but could not be reconciled: %w", host, err)
	}
	s.markUsed(host)
	s.d.bus.Publish(event.Event{Kind: event.KindPod, Pod: s.name, Target: host,
		Detail: "node pod up"})
	native := 0
	for _, h := range np.heldList() {
		if h.Native {
			native++
		}
	}
	s.d.logf("pod %s: node pod on %s at generation %d (%d mounts, %d native)",
		s.name, host, np.generation(), len(np.heldList()), native)
	return nil
}

// rclonePath finds the rclone this machine is using, to copy it rather than ask
// the node's administrator for an install. One binary, in one directory, removable
// with one `rm -rf ~/.vp`.
func rclonePath() (string, error) {
	p, err := exec.LookPath("rclone")
	if err != nil {
		return "", fmt.Errorf("a pod on that machine needs rclone to cache what it "+
			"does not own, and this machine has none to copy: %w", err)
	}
	return p, nil
}

func rcloneAlso(n *remote.Need) string {
	if n.Rclone {
		return " and rclone"
	}
	return ""
}

// requiresFor is everything the mounts of this pod ask of a machine that runs
// them.
func (s *podState) requiresFor(host string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range s.mountList() {
		for _, r := range m.Requires {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	sort.Strings(out)
	return out
}

// needsCacheOn reports whether a pod on this machine would have to cache anything
// locally, which is the only reason rclone has to be there.
func (s *podState) needsCacheOn(host string) bool {
	for _, m := range s.mountList() {
		if m.Owner != route.Pod && m.Owner != host {
			return true
		}
	}
	return false
}

// stopNodePod tells a machine its pod is finished. Best effort by design: a node
// that cannot be reached has a pod holding mounts, and the next `up` adopts or
// clears it rather than this blocking on a machine that may not return.
func (s *podState) stopNodePod(host string) {
	np := s.nodePods.get(host)
	if np == nil {
		return
	}
	s.nodePods.drop(host)
	// Its claim on writing to anything goes with it, so the next machine to take
	// one of those mounts can have it writable; and so do its tunnels.
	s.releaseWriter(host)
	s.dropRelays(host)
	h := s.d.pool.Host(host)
	if _, err := h.Capture(fmt.Sprintf("%s/.vp/bin/vibepod nodedown --pod %s",
		np.home, s.nodeName(host))); err != nil {
		s.d.logf("pod %s: stopping the node pod on %s: %v", s.name, host, err)
	}
}

// needNodePod explains why a command cannot be sent to a machine that has no pod,
// and what to do about it.
//
// The refusal is the point. Sending it anyway would run in a directory of the same
// name on a machine where that name means something else — which succeeds, writes
// somewhere real, and is discovered days later.
func (s *podState) needNodePod(host, cwd string) error {
	owner := route.Owner(s.table(), cwd)
	switch {
	case owner == route.Pod:
		return fmt.Errorf("%s is a directory on this machine, and %s has no pod that "+
			"reproduces it; run this on `pod`, or give %s a pod with "+
			"`vp node add %s`", cwd, host, host, host)
	default:
		return fmt.Errorf("%s belongs to %s, and %s has no pod that reproduces it — a "+
			"command there would be in a directory of the same name on a different "+
			"filesystem; `vp node add %s` builds one", cwd, owner, host, host)
	}
}

// holds reports whether a machine's pod has this directory, which is what makes a
// dispatch to a node that is behind a visible refusal rather than a wrong path.
func (np *nodePod) holds(dir string) bool {
	for _, h := range np.heldList() {
		if under(dir, h.At) {
			return true
		}
	}
	return false
}

// nodeAdd builds a pod on a machine, on request. Typing the command is the
// consent: it is the cheapest form that is still explicit, and it is the same
// grant `vibepod up --push` gives for every machine at once.
func (d *Daemon) nodeAdd(m *proto.Msg, pr *Progress) error {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return err
	}
	host := strings.TrimPrefix(m.Target, "@")
	if host == "" || host == route.Pod {
		return fmt.Errorf("which machine? `vp node add gpu05`")
	}
	if !s.knownBackend(host) {
		return fmt.Errorf("this pod has no machine called %q; `vp hosts` lists them",
			host)
	}
	s.mu.Lock()
	if s.consented == nil {
		s.consented = map[string]bool{}
	}
	s.consented[host] = true
	consented := copySet(s.consented)
	s.mu.Unlock()
	return s.startNodePod(host, consented, pr)
}

func (d *Daemon) nodeDrop(m *proto.Msg) error {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return err
	}
	host := strings.TrimPrefix(m.Target, "@")
	if s.nodePods.get(host) == nil {
		return fmt.Errorf("%s has no pod in this vibepod", host)
	}
	// Any session standing on that machine comes back here: the alternative is a
	// session whose ground has just been removed.
	s.mu.Lock()
	var stranded []string
	for id, b := range s.backends {
		if b == host {
			stranded = append(stranded, id)
		}
	}
	s.mu.Unlock()
	for _, id := range stranded {
		_ = s.setBackend(id, route.Pod)
	}
	s.stopNodePod(host)
	d.bus.Publish(event.Event{Kind: event.KindPod, Pod: s.name, Target: host,
		Detail: "node pod down"})
	return nil
}

func (s *podState) nodeList() []proto.NodeInfo {
	behind := s.staleness()
	out := []proto.NodeInfo{}
	for _, np := range s.nodePods.list() {
		out = append(out, proto.NodeInfo{Host: np.host, Mounts: np.heldList(),
			Generation: np.generation(), Behind: behind[np.host],
			Reachable: np.isReachable()})
	}
	return out
}

// ensureNodePod builds a pod on a machine if this pod already has consent for it,
// which is what makes `vibepod up --push` mean "and keep doing that".
func (s *podState) ensureNodePod(host string) error {
	if s.nodePods.get(host) != nil {
		return nil
	}
	s.mu.Lock()
	consented := copySet(s.consented)
	s.mu.Unlock()
	if !consented[host] {
		return fmt.Errorf("not consented")
	}
	return s.startNodePod(host, consented, nil)
}

func copySet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
