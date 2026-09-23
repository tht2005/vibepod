package daemon

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"os"
	"path"

	"vibepod/internal/config"
	"vibepod/internal/event"
	"vibepod/internal/fs"
	"vibepod/internal/pod"
	"vibepod/internal/proto"
	"vibepod/internal/route"
)

// The mount list is live state, not a boot artifact.
//
// This answers the complaint that shaped v2: adding a machine meant `down`, edit
// the YAML, `up` — losing every session and the agent's context with them. The
// mechanism was already here and unused: vpinit's mount worker holds
// CAP_SYS_ADMIN for the pod's whole life, OpBind is a generic Src→Dst, the FUSE
// manager's Add is incremental, and the ssh pool connects lazily. Only the route
// table was frozen, and it is an atomic pointer now.

// mountRec is one thing mounted in the pod, kept in the order it was added.
// Order is part of the state because it decides what shadows what.
type mountRec struct {
	At         string // where it appears in the pod
	Src        string // host path: a local directory, or the FUSE mountpoint
	Owner      string // route.Pod, or the machine whose disk this is
	RemotePath string // what Owner calls it
	ExecOn     string // a suggestion, surfaced and never applied
	Kind       string // "bind" or the FUSE backend's name
	ReadOnly   bool
	// Requires, Cache and Prefetch travel to a node pod, which is where they
	// mean something: what the machine must have, how much of its disk the cache
	// may take, and whether to fill it up front.
	Requires []string
	Cache    string
	Prefetch bool
	// Gen is the generation this mount entered the list at, so a node holding an
	// older definition of the same path is a fact rather than a guess.
	Gen int64
	// ManyWriters means the config accepted several machines writing into this
	// mount, and with it that the last flush of any one file wins.
	ManyWriters bool
	Runtime     bool // added with `vp mount`, after the pod was up
	// Identity marks the agent's own configuration and credentials. They are
	// the one plane that never leaves this machine.
	Identity bool
	fsMount  *fs.Mount
}

// mountList is the live set, oldest first.
func (s *podState) mountList() []*mountRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*mountRec(nil), s.mounts...)
}

// rebuildRoutes derives the path map from the live mount list. Called under no
// lock and published atomically, so a dispatch in flight sees one table or the
// other and never a half-built one.
func (s *podState) rebuildRoutes() {
	mounts := s.mountList()
	rules := make([]route.Rule, 0, len(mounts))
	for _, m := range mounts {
		rules = append(rules, route.Rule{Prefix: m.At, Owner: m.Owner,
			RemotePath: m.RemotePath, ExecOn: m.ExecOn})
	}
	s.tbl.Store(route.New(s.table().Default, rules))
	s.refreshBrief()
}

// addMount brings up one remote directory and binds it into the pod.
//
// It is the same code path at `up` and an hour later, deliberately: a mount
// added at runtime that behaved differently from one in the config would be a
// second implementation of the only thing this program does.
func (s *podState) addMount(rm proto.MountSpec, pr *Progress, runtime bool) (*mountRec, error) {
	if rm.Mode != "" && rm.Mode != "fuse" {
		return nil, fmt.Errorf("mount mode %q is not implemented yet", rm.Mode)
	}
	at := rm.At
	if at == "" {
		at = rm.Path
	}
	if err := s.checkNewMount(at, rm.Host, rm.Path, runtime); err != nil {
		return nil, err
	}
	if s.fs == nil {
		backend, err := fs.Pick()
		if err != nil {
			return nil, err
		}
		s.fs = fs.NewManager(backend)
	}
	host := s.d.pool.Host(rm.Host)
	pr.step("connecting to %s… ", rm.Host)
	if err := host.Warm(); err != nil {
		pr.failed()
		return nil, err
	}
	pr.ok("connected")

	point := filepath.Join(s.runDir(), "mnt", fmt.Sprintf("%d", s.mountSeq()))
	m := &fs.Mount{Host: rm.Host, RemotePath: rm.Path, MountPoint: point,
		At: at, ReadOnly: rm.ReadOnly, Cache: rm.Cache, Prefetch: rm.Prefetch}
	pr.step("mounting %s:%s via %s… ", rm.Host, rm.Path, s.fs.Backend())
	if err := s.fs.Add(m, host.SSHCommand()); err != nil {
		pr.failed()
		return nil, err
	}
	pr.ok("mounted")

	rec := &mountRec{At: at, Src: point, Owner: rm.Host, RemotePath: rm.Path,
		ExecOn: rm.ExecOn, Kind: s.fs.Backend(), ReadOnly: rm.ReadOnly,
		Requires: rm.Requires, Cache: rm.Cache, Prefetch: rm.Prefetch,
		ManyWriters: rm.ManyWriters, Gen: s.bumpGeneration(),
		Runtime: runtime, fsMount: m}
	if rm.ManyWriters {
		s.mu.Lock()
		if s.manyWriters == nil {
			s.manyWriters = map[string]bool{}
		}
		s.manyWriters[at] = true
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.mounts = append(s.mounts, rec)
	s.mu.Unlock()
	return rec, nil
}

// dropMount forgets a mount whose bind did not take.
func (s *podState) dropMount(rec *mountRec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.mounts {
		if x == rec {
			s.mounts = append(s.mounts[:i], s.mounts[i+1:]...)
			return
		}
	}
}

// bindMount puts a mount into a pod that is already running.
//
// It names the mount by its path in the pod's staging area, not by its path out
// here: the daemon's mount arrived in there by propagation, and a bind whose
// source belongs to another mount namespace is refused by the kernel however it
// is named — an O_PATH descriptor does not help, because the check is on the
// mount rather than the path. See pod.StageDir.
//
// At `up` none of this is needed: the binds travel in the spec and are made
// before pivot_root, while the host's filesystem is still in view.
func (s *podState) bindMount(rec *mountRec) error {
	staged := path.Join(pod.StageDir, filepath.Base(rec.Src))
	reply, err := s.call(&proto.Msg{Op: proto.OpBind, Src: staged, Dst: rec.At,
		ReadOnly: rec.ReadOnly})
	if err != nil {
		if !s.propagates() {
			return fmt.Errorf("%w\n  the filesystem holding %s is not a shared "+
				"mount, so a mount made now cannot reach the pod; this machine can "+
				"still be added with `vibepod down` and `up`", err, s.d.runDir)
		}
		return err
	}
	if reply.Detail != "" {
		s.d.logf("pod %s: %s", s.name, reply.Detail)
	}
	return nil
}

// propagates reports whether a mount made in the daemon's run directory will
// reach a pod. It is the one host property `vp mount` depends on, so it is
// checked and named rather than assumed: systemd mounts everything shared, but a
// machine that does not is entitled to a sentence rather than an EINVAL.
func (s *podState) propagates() bool {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return true // cannot tell; let the real error speak for itself
	}
	best, shared := 0, false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		point := f[4]
		if !under(s.d.runDir, point) || len(point) < best {
			continue
		}
		best = len(point)
		shared = false
		for _, opt := range f[6:] {
			if opt == "-" {
				break
			}
			if strings.HasPrefix(opt, "shared:") {
				shared = true
			}
		}
	}
	return shared
}

// checkNewMount refuses the mounts that cannot be made safe afterwards.
//
// runtime distinguishes a mount being inserted into a live pod from one the
// config asked for. Nesting is legitimate when a file says so — the order is
// written down, and a later mount sitting on top of an earlier one is something
// the author chose. Inserting one into a running pod is not: it changes what the
// mounts above it mean while work is going on inside them, and order is part of
// the state precisely because it decides that. §6a.
func (s *podState) checkNewMount(at, host, path string, runtime bool) error {
	at = filepath.Clean(at)
	if err := config.GuardPath(at); err != nil {
		return err
	}
	for _, m := range s.mountList() {
		if m.At == at {
			return fmt.Errorf("%s is already mounted here, from %s", at, m.source())
		}
		if runtime && (under(at, m.At) || under(m.At, at)) {
			return fmt.Errorf("%s would shadow %s, which is already mounted; "+
				"mount it somewhere else with `at:`, or `vp unmount %s` first",
				at, m.At, m.At)
		}
		// Two caches over the same bytes cannot be made coherent afterwards,
		// so the overlap is refused rather than reported later. §6a.
		if m.Owner == host && path != "" && m.RemotePath != "" &&
			(under(path, m.RemotePath) || under(m.RemotePath, path)) {
			return fmt.Errorf("%s:%s overlaps %s:%s, which is already mounted; "+
				"two caches over the same files cannot be kept coherent",
				host, path, m.Owner, m.RemotePath)
		}
	}
	return nil
}

// removeMount unmounts one mount by the path it occupies in the pod, or by the
// machine it came from.
func (s *podState) removeMount(what string) error {
	mounts := s.mountList()
	var hit []*mountRec
	if host := strings.TrimPrefix(what, "@"); host != what || !strings.HasPrefix(what, "/") {
		for _, m := range mounts {
			if m.Owner == host {
				hit = append(hit, m)
			}
		}
	}
	if len(hit) == 0 {
		at := filepath.Clean(what)
		for _, m := range mounts {
			if m.At == at {
				hit = append(hit, m)
			}
		}
	}
	if len(hit) == 0 {
		return fmt.Errorf("nothing mounted at %q; `vp tree` lists the mounts", what)
	}
	// A session standing in a directory that is about to stop existing is the
	// one failure worth refusing: the alternative is a shell whose cwd is gone.
	for _, m := range hit {
		if sess := s.sessionIn(m.At); sess != "" {
			return fmt.Errorf("session %s is working in %s; move it first "+
				"(`vp cd` elsewhere) and then unmount", sess, m.At)
		}
	}
	for _, m := range hit {
		if _, err := s.call(&proto.Msg{Op: proto.OpUnbind, Dst: m.At}); err != nil {
			return fmt.Errorf("unbind %s in the pod: %w", m.At, err)
		}
		if m.fsMount != nil && s.fs != nil {
			s.fs.Remove(m.fsMount)
		}
		s.mu.Lock()
		for i, x := range s.mounts {
			if x == m {
				s.mounts = append(s.mounts[:i], s.mounts[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		s.d.bus.Publish(event.Event{Kind: event.KindPod, Pod: s.name,
			Target: m.Owner, Detail: "unmounted " + m.At})
		s.d.logf("pod %s: unmounted %s (%s)", s.name, m.At, m.source())
	}
	s.rebuildRoutes()
	// The mounts are gone from here, so they have to go from every node pod too: a
	// node still holding one would be the only holder of a directory this pod no
	// longer has, which is divergence rather than staleness.
	s.bumpGeneration()
	s.reconcileAll("an unmount")
	// Sessions sitting on a machine that is no longer mounted go back to the
	// pod, which is the only answer that cannot be wrong.
	for _, m := range hit {
		if s.knownBackend(m.Owner) {
			continue
		}
		s.mu.Lock()
		var stranded []string
		for id, b := range s.backends {
			if b == m.Owner {
				stranded = append(stranded, id)
			}
		}
		s.mu.Unlock()
		for _, id := range stranded {
			_ = s.setBackend(id, route.Pod)
		}
	}
	return nil
}

// sessionIn reports a session whose shell is standing inside a subtree.
func (s *podState) sessionIn(at string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if under(sess.cwd, at) {
			return id
		}
	}
	return ""
}

// mountAllowed checks a mount request that came from inside the pod.
//
// This is where the boundary moved. Unlike a destructive command, mounting opens
// a network path out of a sandbox whose purpose was containment, so it is the one
// thing the in-pod control plane does not leave unrestricted: a host already in
// the user's ssh config is allowed, anything else is refused.
func (s *podState) mountAllowed(host string) error {
	s.mu.Lock()
	patterns := append([]string(nil), s.canMount...)
	s.mu.Unlock()
	if len(patterns) > 0 {
		for _, p := range patterns {
			if ok, _ := filepath.Match(p, host); ok {
				return nil
			}
		}
		return fmt.Errorf("%s is not in can_mount: %s", host, strings.Join(patterns, ", "))
	}
	if config.InSSHConfig(host) {
		return nil
	}
	return fmt.Errorf("%s is not a Host in your ssh config, so vibepod will not "+
		"reach it from inside a pod; add it there, or list it under can_mount:", host)
}

func (s *podState) runDir() string {
	return filepath.Join(s.d.runDir, "pods", s.name)
}

// mountSeq names the next FUSE mountpoint. Indexed rather than named after the
// host, because several mounts from one machine is ordinary.
func (s *podState) mountSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mountCount++
	return s.mountCount
}

func (m *mountRec) source() string {
	if m.Owner == route.Pod {
		return "local " + m.Src
	}
	return m.Owner + ":" + m.RemotePath
}

// saveState is the live config, in the shape vibepod.yaml wants it.
func (s *podState) saveState() *config.Snapshot {
	snap := &config.Snapshot{Pod: s.name, Default: s.table().Default,
		Path: s.configPath}
	for _, m := range s.mountList() {
		snap.Mounts = append(snap.Mounts, config.SnapMount{
			Local: m.Owner == route.Pod, Host: m.Owner, Path: m.RemotePath,
			Src: m.Src, At: m.At, ReadOnly: m.ReadOnly, ExecOn: m.ExecOn,
		})
	}
	s.mu.Lock()
	for name, host := range s.toolHosts {
		snap.Tools = append(snap.Tools, config.SnapTool{Name: name, Host: host})
	}
	snap.CanMount = append(snap.CanMount, s.canMount...)
	s.mu.Unlock()
	sort.Slice(snap.Tools, func(i, j int) bool { return snap.Tools[i].Name < snap.Tools[j].Name })
	return snap
}

func under(path, prefix string) bool {
	path, prefix = filepath.Clean(path), filepath.Clean(prefix)
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/")
}
