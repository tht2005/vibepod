package fs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Rclone is the preferred backend. Two properties earn it that:
// re-reads come off local disk rather than the wire, and its rc API exposes
// vfs/forget — so vibepod can invalidate the cache when a command finishes
// instead of guessing with a timer.
//
// One Rclone value serves every mount in a pod, so it holds no per-mount state:
// the control address belongs to the Mount. A pod with three mounts from one
// machine is ordinary, and a shared address would send every invalidation to
// whichever of them mounted last.
type Rclone struct{}

func (*Rclone) Name() string { return "rclone" }

func (r *Rclone) Mount(m *Mount, sshCommand string) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	m.rcAddr = fmt.Sprintf("127.0.0.1:%d", port)

	args := []string{
		"mount", ":sftp:" + m.RemotePath, m.MountPoint,
		"--sftp-host", m.Host,
		"--sftp-ssh", sshCommand,
		"--vfs-cache-mode", "full",
		// vibepod is the only thing that touches the remote tree, so the
		// cache can be trusted until a command says otherwise.
		"--dir-cache-time", "8760h",
		"--poll-interval", "0",
		"--rc", "--rc-addr", m.rcAddr, "--rc-no-auth",
		"--daemon",
	}
	if m.ReadOnly {
		args = append(args, "--read-only")
	} else {
		// Writes land on local disk and upload when the file is closed — not on a
		// timer. That is what keeps a checkpoint from stalling a training step,
		// and it is why this one mount also serves as the dataset cache and the
		// re-read cache: three features that would otherwise be three features.
		args = append(args, "--vfs-write-back", "0s")
	}
	if m.Cache != "" {
		// A shared node's scratch disk is not yours to fill. rclone's own default
		// is unbounded, which is fine on your own machine and not fine there.
		args = append(args, "--vfs-cache-max-size", m.Cache)
	}
	out, err := exec.Command("rclone", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Invalidate is the hook the whole backend choice turns on.
// Prefetch copies a tree onto local disk up front.
//
// For exactly one shape: many small files, where a lazy cache is latency-bound on
// the first pass while the machine that asked for the data sits idle. Opt-in,
// because a run that touches one percent of a tree should not pay for all of it.
func (r *Rclone) Prefetch(m *Mount, sshCommand string) error {
	args := []string{"copy", ":sftp:" + m.RemotePath,
		filepath.Join(cacheRoot(m), "prefetch"),
		"--sftp-host", m.Host, "--sftp-ssh", sshCommand,
		"--transfers", "16", "--checkers", "16",
	}
	out, err := exec.Command("rclone", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("prefetch %s:%s: %v: %s", m.Host, m.RemotePath, err,
			strings.TrimSpace(string(out)))
	}
	return nil
}

func cacheRoot(m *Mount) string { return m.MountPoint + ".cache" }

func (r *Rclone) Invalidate(m *Mount, path string) error {
	if m.rcAddr == "" {
		return ErrNoInvalidate
	}
	body, _ := json.Marshal(map[string]string{"dir": strings.TrimPrefix(path, "/")})
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post("http://"+m.rcAddr+"/vfs/forget",
		"application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vfs/forget: %s", resp.Status)
	}
	return nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
