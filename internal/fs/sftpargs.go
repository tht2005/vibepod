package fs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// How rclone reaches an sftp server.
//
// The good way is --sftp-ssh: rclone runs the system ssh, so the user's ssh config
// — aliases, ProxyJump, keys, the multiplexed connection — applies exactly as it
// does to everything else. It arrived in rclone 1.56, and the distribution rclone
// on a real server is often older: aiotlab's is 1.53. There, vibepod resolves the
// alias itself with `ssh -G` — the same answer ssh would use — and hands rclone the
// pieces. A jump host cannot be expressed that way, and that is refused by name
// rather than failing as a connection timeout.

var (
	sshFlagOnce sync.Once
	sshFlagOK   bool
)

// rcloneHasSSHFlag asks the installed rclone once.
func rcloneHasSSHFlag() bool {
	sshFlagOnce.Do(func() {
		out, err := exec.Command("rclone", "help", "flags", "sftp").CombinedOutput()
		sshFlagOK = err == nil && strings.Contains(string(out), "--sftp-ssh")
	})
	return sshFlagOK
}

func sftpArgs(m *Mount, sshCommand string) ([]string, error) {
	if e := m.Endpoint; e != nil {
		args := []string{"--sftp-host", e.Host, "--sftp-port", fmt.Sprint(e.Port),
			"--sftp-user", e.User}
		if e.KeyFile != "" {
			args = append(args, "--sftp-key-file", e.KeyFile)
		}
		return args, nil
	}
	if rcloneHasSSHFlag() {
		return []string{"--sftp-host", m.Host, "--sftp-ssh", sshCommand}, nil
	}
	t, err := resolveSSH(sshCommand, m.Host)
	if err != nil {
		return nil, err
	}
	args := []string{"--sftp-host", t.host, "--sftp-port", t.port, "--sftp-user", t.user}
	if t.key != "" {
		args = append(args, "--sftp-key-file", t.key)
	} else {
		args = append(args, "--sftp-key-use-agent")
	}
	return args, nil
}

type sshTarget struct{ host, port, user, key string }

// resolveSSH is `ssh -G`: what ssh itself would connect to for an alias, with the
// same config and options vibepod passes everywhere else.
func resolveSSH(sshCommand, alias string) (*sshTarget, error) {
	fields := strings.Fields(sshCommand)
	if len(fields) == 0 {
		fields = []string{"ssh"}
	}
	out, err := exec.Command(fields[0], append(fields[1:], "-G", alias)...).Output()
	if err != nil {
		return nil, fmt.Errorf("resolve %s with ssh -G: %w", alias, err)
	}
	t := &sshTarget{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		switch k {
		case "hostname":
			t.host = v
		case "port":
			t.port = v
		case "user":
			t.user = v
		case "identityfile":
			if t.key == "" {
				p := v
				if strings.HasPrefix(p, "~/") {
					home, _ := os.UserHomeDir()
					p = filepath.Join(home, p[2:])
				}
				if _, err := os.Stat(p); err == nil {
					t.key = p
				}
			}
		case "proxyjump", "proxycommand":
			if v != "none" && v != "" {
				return nil, fmt.Errorf("%s is reached through %s, which this machine's "+
					"rclone cannot follow — it predates --sftp-ssh (1.56). Install "+
					"sshfs or a newer rclone here", alias, k)
			}
		}
	}
	if t.host == "" {
		return nil, fmt.Errorf("ssh -G gave no hostname for %s", alias)
	}
	return t, nil
}
