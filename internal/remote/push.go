package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// The remote footprint, and what it is allowed to be.
//
// Two things may ever be copied to a machine: the vibepod binary, and rclone if
// the machine has none. Nothing else, and never a credential — that is the
// promise §1 is built on, so it is enforced here rather than remembered.
//
// The reason a binary has to go at all is honest and worth stating: caching a
// dataset on a node's own disk requires a process on that node. There is no
// arrangement of ssh that puts bytes on gpu05's NVMe without something running
// there to write them.

// Need is what a machine is missing before it can be a backend with a pod.
type Need struct {
	Binary bool // vibepod itself
	// Rclone is set only when a mount has to be *cached* on that machine and the
	// machine has no FUSE backend of its own. A machine that already has rclone or
	// sshfs needs nothing copied: vibepod uses what is there, which is the same
	// principle as using the node's own toolchain.
	Rclone   bool
	Missing  []string // requires: entries the machine does not have
	Userns   bool     // unprivileged user namespaces are unavailable
	HomeDir  string
	Existing string // the version already there, if any
}

// Probe asks a machine what it has, in one round trip. Asking separately would be
// four round trips on a link whose latency is the thing being measured.
//
// What is already there is identified by *hash*, not by version. A version string
// cannot tell one build of a development tree from another, and the symptom of
// getting that wrong is a node quietly behaving like an older build — which is
// the same bad hour the daemon's own build check exists to prevent, one machine
// further out.
func (h *Host) Probe(selfPath string, needRclone bool, requires []string) (*Need, error) {
	want, err := fileHash(selfPath)
	if err != nil {
		return nil, err
	}
	script := `printf 'home=%s\n' "$HOME"
b="$HOME/.vp/bin/vibepod"
if [ -x "$b" ]; then
  if command -v sha256sum >/dev/null 2>&1; then printf 'hash=%s\n' "$(sha256sum "$b" | cut -d" " -f1)"
  elif command -v shasum >/dev/null 2>&1; then printf 'hash=%s\n' "$(shasum -a 256 "$b" | cut -d" " -f1)"
  else printf 'hash=%s\n' unknown; fi
fi
for b in rclone sshfs; do
  command -v "$b" >/dev/null 2>&1 && echo backend=yes
  [ -x "$HOME/.vp/bin/$b" ] && echo backend=yes
done
u=$(cat /proc/sys/user/max_user_namespaces 2>/dev/null || echo 1); [ "$u" = 0 ] && echo userns=no
`
	for _, r := range requires {
		script += fmt.Sprintf("[ -e %s ] || printf 'missing=%%s\\n' %s\n", quote(r), quote(r))
	}
	args := append(h.Opts(), h.Alias, "sh -c "+quote(script))
	out, err := exec.Command("ssh", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s", Explain(h.Alias, string(out)))
	}
	n := &Need{Binary: true, Rclone: needRclone}
	n.Existing = "nothing"
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "home":
			n.HomeDir = value
		case "hash":
			n.Existing = value
			if value == want {
				n.Binary = false
			}
		case "backend":
			n.Rclone = false
		case "userns":
			n.Userns = true
		case "missing":
			n.Missing = append(n.Missing, value)
		}
	}
	return n, nil
}

// fileHash is what the machine's copy is compared against.
func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Push copies one file into ~/.vp/bin on a machine, atomically enough that a
// binary being replaced under a running pod is not observed half-written.
func (h *Host) Push(localPath, name string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp := fmt.Sprintf("$HOME/.vp/bin/.%s.new", name)
	script := fmt.Sprintf(`mkdir -p "$HOME/.vp/bin" && cat > %s && chmod 755 %s && `+
		`mv -f %s "$HOME/.vp/bin/%s"`, tmp, tmp, tmp, name)
	args := append(h.Opts(), h.Alias, "sh -c "+quote(script))
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push %s to %s: %v: %s", name, h.Alias, err,
			strings.TrimSpace(string(out)))
	}
	return nil
}

// WriteFile puts a small file on a machine, for a node pod's spec.
func (h *Host) WriteFile(remotePath string, content []byte) error {
	script := fmt.Sprintf(`mkdir -p "$(dirname %s)" && cat > %s`,
		quote(remotePath), quote(remotePath))
	args := append(h.Opts(), h.Alias, "sh -c "+quote(script))
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = strings.NewReader(string(content))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("write %s on %s: %v: %s", remotePath, h.Alias, err,
			strings.TrimSpace(string(out)))
	}
	return nil
}

// Capture runs one command on a machine and returns its output, for the short
// control exchanges: starting a node pod, stopping one, asking what it holds.
func (h *Host) Capture(command string) (string, error) {
	args := append(h.Opts(), h.Alias, command)
	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %v: %s", h.Alias, err,
			strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
