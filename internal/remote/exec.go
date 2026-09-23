package remote

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// InPod returns a view of this machine whose commands run inside its node pod
// rather than on the machine itself. Everything else — the multiplexed
// connection, the descriptors, the pid file, the signal path — is unchanged: a
// node pod is a namespace, not a different transport.
func (h *Host) InPod(home, pod string) *Host {
	c := *h
	c.enter = fmt.Sprintf("%s/.vp/bin/vibepod nodeexec --pod %s --", home, pod)
	return &c
}

// InPod reports whether this view enters a node pod.
func (h *Host) IsInPod() bool { return h.enter != "" }

// Req is one command to run on a remote machine.
type Req struct {
	Dir  string   // working directory, as it exists on that machine
	Argv []string // argv[0] is resolved by the remote's own PATH
	Env  []string // VAR=value assignments the caller set for this command
	TTY  bool
	ID   string // names the pid file, so the command can be signalled
	// Files are the caller's own stdin, stdout and stderr. The daemon wires
	// them straight to ssh and never sits in the data path.
	Files [3]*os.File
}

// runDir is where a routed command records its pid on the remote. It is state,
// not an installation: nothing is copied there and `vpctl down` removes it.
const runDirExpr = `"${TMPDIR:-/tmp}/vibepod-$(id -u)"`

// Run executes the command and returns its exit status.
//
// The remote runs `exec` as its last act, so the pid recorded in the pid file
// is the command's own and a second, multiplexed channel can signal it. That
// is what makes Ctrl-C work without a PTY, which agents need because they read
// stdout and stderr separately.
func (h *Host) Run(r Req) (int, error) {
	// Assignments go before `exec`, not after: `exec VAR=v cmd` would have the
	// shell look for a program called "VAR=v". Before it, they are exported to
	// the exec'd process, and the pid is still the shell's — so the pid file
	// above still names the command, and signals still reach it.
	// Inside a node pod the command is handed to `nodeexec`, which passes these
	// same descriptors on to the pod and waits. The pid file still names the
	// shell, so the signal path below is unchanged.
	command := joinArgs(r.Argv)
	if h.enter != "" {
		command = h.enter + " " + command
	}
	// An empty Dir means "this machine's own home": the caller's directory was one
	// this machine does not have, and a command that does not care where it runs
	// should run rather than be refused. Which it was is reported; see the daemon.
	cd := "cd " + quote(r.Dir) + " || exit 1"
	if r.Dir == "" {
		cd = "cd || exit 1"
	}
	// ~/.vp/bin goes on the *end* of PATH: it holds what vibepod was allowed to
	// copy there (see toolbin), and a tool the machine has of its own must always
	// win over a copy.
	script := fmt.Sprintf(`d=%s; mkdir -p "$d"; echo $$ > "$d/%s"; %s; `+
		`PATH="$PATH:$HOME/.vp/bin"; export PATH; %sexec %s`,
		runDirExpr, r.ID, cd, assignments(r.Env), command)

	args := h.Opts()
	if r.TTY {
		args = append(args, "-tt")
	} else {
		args = append(args, "-T")
	}
	args = append(args, h.Alias, "sh -c "+quote(script))

	cmd := exec.Command("ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.Files[0], r.Files[1], r.Files[2]
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		return ExitLinkDown, fmt.Errorf("%s: %w", h.Alias, err)
	}
	code := ee.ExitCode()
	// ssh reports its own failures as 255. Distinguish "the link broke" from
	// "the command failed", because the agent must not treat them alike.
	if code == 255 {
		return ExitLinkDown, fmt.Errorf("link to %s dropped", h.Alias)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return code, nil
}

// Signal delivers a signal to a running remote command over the existing
// multiplexed connection.
func (h *Host) Signal(id string, sig int) error {
	script := fmt.Sprintf(`d=%s; p=$(cat "$d/%s" 2>/dev/null) && kill -%d "$p" 2>/dev/null`,
		runDirExpr, id, sig)
	args := append(h.Opts(), h.Alias, "sh -c "+quote(script))
	return exec.Command("ssh", args...).Run()
}

// Cleanup removes the pid files this pod left behind.
func (h *Host) Cleanup() error {
	script := fmt.Sprintf(`rm -rf %s`, runDirExpr)
	args := append(h.Opts(), h.Alias, "sh -c "+quote(script))
	return exec.Command("ssh", args...).Run()
}

// quote makes a string safe for a POSIX shell.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// assignments renders VAR=value pairs for the front of a command, quoting the
// values and leaving the names alone — a name that needed quoting would not be
// a name.
func assignments(env []string) string {
	if len(env) == 0 {
		return ""
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" || !validName(name) {
			continue
		}
		out = append(out, name+"="+quote(value))
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " ") + " "
}

// validName guards the one place a value's syntax reaches the remote shell
// unquoted.
func validName(name string) bool {
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func joinArgs(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = quote(a)
	}
	return strings.Join(out, " ")
}

// Children lists what is running under a dispatched command, by polling `ps` over
// the multiplexed connection. Polled, so approximate: a process that started and
// ended between two looks is not there, and one inside a node pod's own pid
// namespace appears under that pod rather than under this command.
func (h *Host) Children(id string) ([]string, error) {
	script := fmt.Sprintf(`d=%s; p=$(cat "$d/%s" 2>/dev/null) || exit 0; `+
		`ps -o pid=,args= --ppid "$p" 2>/dev/null; exit 0`, runDirExpr, id)
	args := append(h.Opts(), h.Alias, "sh -c "+quote(script))
	out, err := exec.Command("ssh", args...).Output()
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}
