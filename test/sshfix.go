package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// sshFixture is a real sshd on a high port, so the remote tests exercise the
// actual transport — ControlMaster, sftp, exit codes — rather than a mock of
// it. It runs as the test user and touches nothing outside its own directory.
type sshFixture struct {
	dir        string
	configFile string // ssh_config defining the vptest aliases
	binDir     string // on the fixture host's PATH: tools the pod does not have
	port       int
	cmd        *exec.Cmd
}

func startSSHD(dir string, port int) (*sshFixture, error) {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		if _, e := os.Stat("/usr/bin/sshd"); e != nil {
			return nil, fmt.Errorf("no sshd available")
		}
		sshd = "/usr/bin/sshd"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	run := func(name string, args ...string) error {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %v: %s", name, err, out)
		}
		return nil
	}
	host := filepath.Join(dir, "hostkey")
	id := filepath.Join(dir, "id")
	if err := run("ssh-keygen", "-q", "-t", "ed25519", "-f", host, "-N", ""); err != nil {
		return nil, err
	}
	if err := run("ssh-keygen", "-q", "-t", "ed25519", "-f", id, "-N", ""); err != nil {
		return nil, err
	}
	pub, err := os.ReadFile(id + ".pub")
	if err != nil {
		return nil, err
	}
	authKeys := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(authKeys, pub, 0o600); err != nil {
		return nil, err
	}
	sftp := "/usr/lib/ssh/sftp-server"
	for _, c := range []string{"/usr/lib/ssh/sftp-server", "/usr/libexec/sftp-server",
		"/usr/lib/openssh/sftp-server", "/usr/lib/ssh/sftp-server"} {
		if _, err := os.Stat(c); err == nil {
			sftp = c
			break
		}
	}
	// A directory on the fixture host's PATH, and nowhere on this machine's, so
	// that "a tool that exists only on the remote" is a real statement rather
	// than a stand-in. sshd puts it on the session's PATH with SetEnv.
	fixBin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(fixBin, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(fixBin, "vp-only-tool"),
		[]byte("#!/bin/sh\necho only-on-the-remote \"$@\"\n"), 0o755); err != nil {
		return nil, err
	}
	cfg := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(
		"Port %d\nListenAddress 127.0.0.1\nHostKey %s\nAuthorizedKeysFile %s\n"+
			"PidFile %s/sshd.pid\nStrictModes no\nUsePAM no\nPrintMotd no\n"+
			"X11Forwarding no\nSubsystem sftp %s\n"+
			"SetEnv PATH=%s:/usr/local/bin:/usr/bin:/bin\n",
		port, host, authKeys, dir, sftp, fixBin)), 0o600); err != nil {
		return nil, err
	}
	// Two aliases for one sshd. The plural cases are the interesting ones — a
	// tool two machines could run, a second backend mounted at runtime — and
	// they need two names, not two machines.
	sshCfg := filepath.Join(dir, "ssh_config")
	alias := func(name string) string {
		return fmt.Sprintf("Host %s\n  HostName 127.0.0.1\n  Port %d\n  User %s\n"+
			"  IdentityFile %s\n  IdentitiesOnly yes\n"+
			"  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n",
			name, port, os.Getenv("USER"), id)
	}
	if err := os.WriteFile(sshCfg,
		[]byte(alias("vptest")+alias("vptest2")), 0o600); err != nil {
		return nil, err
	}

	cmd := exec.Command(sshd, "-D", "-f", cfg, "-E", filepath.Join(dir, "sshd.log"))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	f := &sshFixture{dir: dir, configFile: sshCfg, binDir: fixBin, port: port,
		cmd: cmd}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("ssh", "-F", sshCfg, "-o", "ConnectTimeout=2",
			"vptest", "true").Run() == nil {
			return f, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.stop()
	return nil, fmt.Errorf("sshd did not accept connections; see %s/sshd.log", dir)
}

func (f *sshFixture) stop() {
	if f.cmd != nil && f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
		_, _ = f.cmd.Process.Wait()
	}
}
