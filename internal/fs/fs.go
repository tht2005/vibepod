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
)

// Mount is one remote directory, mounted on the host.
type Mount struct {
	Host       string // ssh_config alias
	RemotePath string
	MountPoint string // host path where it is mounted
	At         string // where it appears in the pod
	ReadOnly   bool
	backend    Backend
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
// host, so anything mounted from it may have changed and nothing else could
// have changed it.
func (mg *Manager) InvalidateAfter(host string) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	for _, m := range mg.mounts {
		if m.Host != host {
			continue
		}
		if err := mg.backend.Invalidate(m, "/"); err != nil && err != ErrNoInvalidate {
			continue
		}
	}
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
}

// Mounts lists what is mounted, for vpctl tree.
func (mg *Manager) Mounts() []*Mount {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return append([]*Mount(nil), mg.mounts...)
}

func hostSpec(m *Mount) string { return m.Host + ":" + m.RemotePath }

func combine(opts []string) string { return strings.Join(opts, ",") }
