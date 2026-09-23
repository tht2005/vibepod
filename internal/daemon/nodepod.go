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
	// mounts is what that node holds, so a dispatch into a mount it does not
	// have can be refused by name rather than run against the wrong bytes.
	mounts []string
	native map[string]bool // mounts it owns, and therefore binds natively
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
		Pod:      s.name,
		Node:     host,
		RunDir:   node.RunDir(home, s.name),
		Hostname: host,
		SSHExtra: s.nodeSSHExtra(),
		Version:  Version,
	}
	for _, m := range s.mountList() {
		if m.Identity {
			continue
		}
		if m.Owner == route.Pod {
			// A directory on *this* machine. Reaching it from a node means
			// serving it back out — which is the reverse mount, and is not built.
			// Refuse at `up`, where a person is watching, and say what a node pod
			// on this config would and would not see.
			return nil, fmt.Errorf("%s is a directory on this machine, and a pod on "+
				"%s cannot reach it yet; mount it from a machine %s can see, or run "+
				"that session on `pod`", m.At, host, host)
		}
		spec.Mounts = append(spec.Mounts, proto.MountSpec{
			At: m.At, Host: m.Owner, Path: m.RemotePath, ReadOnly: m.ReadOnly,
			Cache: m.Cache, Prefetch: m.Prefetch,
		})
		if m.Cache != "" {
			spec.Cache = m.Cache
		}
	}
	if len(spec.Mounts) == 0 {
		return nil, fmt.Errorf("a pod on %s would hold nothing: every mount in this "+
			"pod is local to this machine", host)
	}
	return spec, nil
}

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

	np := &nodePod{host: host, sock: sock, home: need.HomeDir,
		native: map[string]bool{}}
	for _, m := range spec.Mounts {
		np.mounts = append(np.mounts, m.At)
		if m.Host == host {
			np.native[m.At] = true
		}
	}
	s.nodePods.put(host, np)
	s.markUsed(host)
	s.d.bus.Publish(event.Event{Kind: event.KindPod, Pod: s.name, Target: host,
		Detail: "node pod up"})
	s.d.logf("pod %s: node pod on %s (%d mounts, %d native)", s.name, host,
		len(np.mounts), len(np.native))
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
	h := s.d.pool.Host(host)
	if _, err := h.Capture(fmt.Sprintf("%s/.vp/bin/vibepod nodedown --pod %s",
		np.home, s.name)); err != nil {
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

// nodeHolds reports whether a machine's pod has this directory, which is what
// makes a dispatch to a stale node a visible refusal rather than a wrong path.
func (np *nodePod) holds(dir string) bool {
	for _, at := range np.mounts {
		if under(dir, at) {
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
	out := []proto.NodeInfo{}
	for _, np := range s.nodePods.list() {
		info := proto.NodeInfo{Host: np.host, Mounts: np.mounts}
		for at := range np.native {
			info.Native = append(info.Native, at)
		}
		sort.Strings(info.Native)
		out = append(out, info)
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
