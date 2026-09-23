// Package fs mounts remote directories.
//
// Every mount is made on the host by the daemon and bound into the pod. That
// is forced — setuid is inert inside a pod, and fusermount3 is setuid — but it
// yields a property worth having: the agent cannot unmount, remount or tamper
// with any mount, because there is no fusermount available to it.
package fs

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mount is one remote directory, mounted on the host.
type Mount struct {
	Host       string // ssh_config alias
	RemotePath string
	MountPoint string // host path where it is mounted
	At         string // where it appears in the pod
	ReadOnly   bool
	// Cache bounds the on-disk cache, as rclone spells a size ("200G"). Empty
	// leaves rclone's own default, which is unbounded — fine on your own machine,
	// not fine on a shared node's scratch disk.
	Cache string
	// Prefetch copies the tree up front instead of warming it lazily. For the one
	// shape a lazy cache handles badly: a first epoch over a million small files
	// is latency-bound while the GPUs idle.
	Prefetch bool
	backend  Backend
	// rcAddr is this mount's own control address. Per mount, not per backend: a
	// pod with three mounts runs three rclone processes, and one shared field
	// would leave every invalidation going to whichever mounted last.
	rcAddr string
}

// Backend is a way of making a remote directory readable locally. The choice
// matters for one reason above all: whether it can be told to forget what it
// cached. vibepod mediates every command, so it knows exactly when a remote
// tree may have changed — a cache that can be invalidated on command
// completion can be far more aggressive than any general network filesystem.
type Backend interface {
	Name() string
	Mount(m *Mount, sshCommand string) error
	// Invalidate drops cached state for a subtree. A backend without the
	// hook reports ErrNoInvalidate and is used with conservative timeouts.
	Invalidate(m *Mount, path string) error
}

var ErrNoInvalidate = fmt.Errorf("backend cannot be told to forget cached state")

// Flusher is a backend with a write-back cache that can be asked whether it has
// finished writing. Only rclone has one.
type Flusher interface {
	Flush(m *Mount, timeout time.Duration) error
}

// Flush waits for a mount's unwritten data to reach the machine that owns it.
//
// This is what makes unmounting safe rather than fast. A write-back cache means
// the node holds bytes that exist nowhere else — a checkpoint written a second ago
// — and unmounting without flushing would discard them silently, which would be
// the worst bug in this program. A backend with no cache has nothing to wait for
// and says so by not implementing this.
func (mg *Manager) Flush(m *Mount, timeout time.Duration) error {
	f, ok := mg.backend.(Flusher)
	if !ok {
		return nil
	}
	return f.Flush(m, timeout)
}

// Prefetcher is a backend that can fill its cache up front. Only rclone can, and
// only the one shape needs it: many small files, where a lazy cache is
// latency-bound on the first pass while the machine that asked for the data idles.
type Prefetcher interface {
	Prefetch(m *Mount, sshCommand string) error
}

// Prefetch fills the cache for a mount that asked for it, and reports whether the
// backend could. A backend that cannot is not a failure: the mount works, the
// first pass is merely slower, and saying so belongs to the caller.
func (mg *Manager) Prefetch(m *Mount, sshCommand string) (bool, error) {
	p, ok := mg.backend.(Prefetcher)
	if !ok || !m.Prefetch {
		return false, nil
	}
	return true, p.Prefetch(m, sshCommand)
}

// Pick chooses the best backend available on this machine. rclone is
// preferred because it alone supports execution-aware invalidation; sshfs is
// the fallback that is almost always already installed.
func Pick() (Backend, error) {
	if _, err := exec.LookPath("rclone"); err == nil {
		return &Rclone{}, nil
	}
	if _, err := exec.LookPath("sshfs"); err == nil {
		return &Sshfs{}, nil
	}
	return nil, fmt.Errorf("no filesystem backend: install rclone (preferred) or sshfs")
}

// Manager owns the mounts of one pod.
type Manager struct {
	backend Backend

	mu     sync.Mutex
	mounts []*Mount
}

func NewManager(b Backend) *Manager { return &Manager{backend: b} }

func (mg *Manager) Backend() string { return mg.backend.Name() }

// Add mounts a remote directory at mountPoint.
func (mg *Manager) Add(m *Mount, sshCommand string) error {
	// A daemon that died without unmounting leaves a mountpoint whose server
	// is gone; every access to it fails until it is detached. Clear it first
	// rather than reporting a confusing permission error.
	unmountPoint(m.MountPoint)
	if err := os.MkdirAll(m.MountPoint, 0o755); err != nil {
		return err
	}
	m.backend = mg.backend
	if err := mg.backend.Mount(m, sshCommand); err != nil {
		return fmt.Errorf("mount %s:%s: %w", m.Host, m.RemotePath, err)
	}
	mg.mu.Lock()
	mg.mounts = append(mg.mounts, m)
	mg.mu.Unlock()
	return nil
}

// InvalidateAfter is the execution-aware part: a command has just finished on
// host, so anything mounted from it may have changed and nothing else could have
// changed it.
//
// Per host was right when there was one cache. With a pod on every backend there
// are several, and the grain matters twice: the daemon has to reach the other
// holders too (see the reconciler), and throwing away an unrelated mount's cache
// because a command touched a different one on the same machine is waste.
func (mg *Manager) InvalidateAfter(host string) {
	for _, m := range mg.Mounts() {
		if m.Host == host {
			mg.InvalidateMount(m)
		}
	}
}

// InvalidateMount forgets one mount's cached state.
func (mg *Manager) InvalidateMount(m *Mount) {
	if err := mg.backend.Invalidate(m, "/"); err != nil && err != ErrNoInvalidate {
		// Nothing to do about it here: the cache is merely stale, which the
		// backend's own timeouts will eventually correct. The caller has no better
		// recovery than that either.
		_ = err
	}
}

// MountsAt returns the mount occupying a path in the pod, if any.
func (mg *Manager) MountAt(at string) *Mount {
	for _, m := range mg.Mounts() {
		if m.At == at {
			return m
		}
	}
	return nil
}

// Remove releases one mount, for `vp unmount`. The pod's bind is detached by
// vpinit first; this is the FUSE mount underneath it.
func (mg *Manager) Remove(m *Mount) {
	mg.mu.Lock()
	for i, x := range mg.mounts {
		if x == m {
			mg.mounts = append(mg.mounts[:i], mg.mounts[i+1:]...)
			break
		}
	}
	mg.mu.Unlock()
	unmountPoint(m.MountPoint)
}

// Unmount releases every mount, lazily so that a dead link does not wedge
// shutdown.
func (mg *Manager) Unmount() {
	mg.mu.Lock()
	mounts := mg.mounts
	mg.mounts = nil
	mg.mu.Unlock()
	// Deepest first, so a nested mount never blocks its parent.
	sort.Slice(mounts, func(i, j int) bool {
		return len(mounts[i].MountPoint) > len(mounts[j].MountPoint)
	})
	for _, m := range mounts {
		unmountPoint(m.MountPoint)
	}
}

// UnmountUnder releases every FUSE mount beneath a directory, for the case where
// the process that made them is gone and there is nobody left who knows what they
// were. A mountpoint whose server has died fails every access until it is
// detached, so this is cleanup rather than tidiness.
func UnmountUnder(dir string) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return
	}
	var points []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		p := f[4]
		if p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/") {
			points = append(points, p)
		}
	}
	// Deepest first, so a nested mount never blocks its parent.
	sort.Slice(points, func(i, j int) bool { return len(points[i]) > len(points[j]) })
	for _, p := range points {
		unmountPoint(p)
	}
}

// unmountPoint detaches a FUSE mount lazily, so a dead link cannot wedge
// shutdown. fusermount3 is the setuid helper on the host; the pod has no
// access to it, which is exactly why mounts live out here.
func unmountPoint(point string) {
	for _, bin := range []string{"fusermount3", "fusermount"} {
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		if exec.Command(bin, "-u", "-z", point).Run() == nil {
			return
		}
	}
	// A mount whose server has already died is not in the helper's table any more,
	// and fusermount then refuses to touch it — leaving a directory that fails
	// every access and cannot even be removed. plain umount still detaches it, and
	// this is the one case that needs it.
	_ = exec.Command("umount", point).Run()
}

// Mounts lists what is mounted, for vpctl tree.
func (mg *Manager) Mounts() []*Mount {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return append([]*Mount(nil), mg.mounts...)
}

func hostSpec(m *Mount) string { return m.Host + ":" + m.RemotePath }

func combine(opts []string) string { return strings.Join(opts, ",") }
