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

	var args []string
	if m.LocalHop {
		args = []string{"mount", m.RemotePath, m.MountPoint}
	} else {
		conn, err := sftpArgs(m, sshCommand)
		if err != nil {
			return err
		}
		args = append([]string{"mount", ":sftp:" + m.RemotePath, m.MountPoint}, conn...)
	}
	args = append(args,
		"--vfs-cache-mode", "full",
		// vibepod is the only thing that touches the remote tree, so the
		// cache can be trusted until a command says otherwise.
		"--dir-cache-time", "8760h",
		"--poll-interval", "0",
		"--rc", "--rc-addr", m.rcAddr, "--rc-no-auth",
		"--daemon",
	)
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
// Flush waits until this mount has written everything back.
//
// rclone's VFS uploads on file close rather than on a timer, so "has it finished"
// is a question with an answer: the transfer queue. Asking it is the difference
// between an unmount that is safe and one that is merely quick.
func (r *Rclone) Flush(m *Mount, timeout time.Duration) error {
	if m.rcAddr == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		queued, err := r.pending(m)
		if err != nil {
			// Cannot ask. Saying "flushed" would be a guess about somebody's
			// checkpoint, so it is a refusal instead.
			return fmt.Errorf("cannot ask %s whether it has finished writing: %w",
				m.MountPoint, err)
		}
		if queued == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d file(s) still waiting to be written to %s:%s",
				queued, m.Host, m.RemotePath)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// pending is how many writes this mount's rclone has not finished.
//
// vfs/stats says so directly, and arrived in rclone 1.54. Older ones — the
// distribution rclone on a real server is often older — have only core/stats,
// whose "transferring" list is the uploads in flight. Each mount is its own rclone
// process and write-back uploads as soon as a file is closed, so for one mount
// that list is the same answer.
func (r *Rclone) pending(m *Mount) (int, error) {
	if stats, err := r.rc(m, "vfs/stats", nil); err == nil {
		return vfsQueued(stats), nil
	}
	stats, err := r.rc(m, "core/stats", nil)
	if err != nil {
		return 0, err
	}
	n := 0
	if list, ok := stats["transferring"].([]any); ok {
		n += len(list)
	}
	return n, nil
}

// vfsQueued digs the outstanding-write count out of rclone's reply. The shape of
// that reply has changed between versions, so this reads every plausible field
// rather than one: over-reporting delays an unmount, under-reporting loses a file.
func vfsQueued(stats map[string]any) int {
	total := 0
	for _, key := range []string{"uploadsInProgress", "uploadsQueued"} {
		if v, ok := stats[key].(float64); ok {
			total += int(v)
		}
	}
	if disk, ok := stats["diskCache"].(map[string]any); ok {
		for _, key := range []string{"uploadsInProgress", "uploadsQueued"} {
			if v, ok := disk[key].(float64); ok {
				total += int(v)
			}
		}
	}
	return total
}

// rc calls one of rclone's control endpoints on the process serving this mount.
func (r *Rclone) rc(m *Mount, path string, body map[string]string) (map[string]any, error) {
	if body == nil {
		body = map[string]string{}
	}
	blob, _ := json.Marshal(body)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post("http://"+m.rcAddr+"/"+path, "application/json",
		bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", path, resp.Status)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Prefetch copies a tree onto local disk up front.
//
// For exactly one shape: many small files, where a lazy cache is latency-bound on
// the first pass while the machine that asked for the data sits idle. Opt-in,
// because a run that touches one percent of a tree should not pay for all of it.
func (r *Rclone) Prefetch(m *Mount, sshCommand string) error {
	conn, err := sftpArgs(m, sshCommand)
	if err != nil {
		return err
	}
	args := append([]string{"copy", ":sftp:" + m.RemotePath,
		filepath.Join(cacheRoot(m), "prefetch"), "--transfers", "16",
		"--checkers", "16"}, conn...)
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
