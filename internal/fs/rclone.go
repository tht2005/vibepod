package fs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Rclone is the preferred backend. Two properties earn it that:
// re-reads come off local disk rather than the wire, and its rc API exposes
// vfs/forget — so vibepod can invalidate the cache when a command finishes
// instead of guessing with a timer.
type Rclone struct {
	rcAddr string
	cmd    *exec.Cmd
}

func (*Rclone) Name() string { return "rclone" }

func (r *Rclone) Mount(m *Mount, sshCommand string) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	r.rcAddr = fmt.Sprintf("127.0.0.1:%d", port)

	args := []string{
		"mount", ":sftp:" + m.RemotePath, m.MountPoint,
		"--sftp-host", m.Host,
		"--sftp-ssh", sshCommand,
		"--vfs-cache-mode", "full",
		// vibepod is the only thing that touches the remote tree, so the
		// cache can be trusted until a command says otherwise.
		"--dir-cache-time", "8760h",
		"--poll-interval", "0",
		"--rc", "--rc-addr", r.rcAddr, "--rc-no-auth",
		"--daemon",
	}
	if m.ReadOnly {
		args = append(args, "--read-only")
	}
	out, err := exec.Command("rclone", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Invalidate is the hook the whole backend choice turns on.
func (r *Rclone) Invalidate(m *Mount, path string) error {
	if r.rcAddr == "" {
		return ErrNoInvalidate
	}
	body, _ := json.Marshal(map[string]string{"dir": strings.TrimPrefix(path, "/")})
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post("http://"+r.rcAddr+"/vfs/forget",
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
