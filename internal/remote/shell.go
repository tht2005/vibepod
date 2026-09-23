package remote

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// A live shell on another machine.
//
// This is the part of v2 that makes an attached session on gpu03 feel like
// gpu03: it is not a proxy and not one ssh per command, but a single shell that
// lives as long as the session does. `cd` sticks because it is `cd`. `export`
// sticks, `jobs` and `fg` work, history is that machine's history — none of it
// reproduced, all of it simply present.
//
// v1's per-exec ssh could not do this, and that is most of why cwd-routing felt
// broken. See DESIGN.md §3.

// Shell starts a login shell on this machine, with the two descriptors the
// caller passes wired to it. The caller owns a pty and hands us the slave, so
// ssh sees a terminal, asks the far side for one, and forwards window changes
// on its own.
//
// dir is where the shell should start. It is advisory: a machine that does not
// have that directory gets the user's home instead, with a line saying so,
// rather than a shell that refuses to open.
func (h *Host) Shell(dir string, tty *os.File) (*exec.Cmd, error) {
	if tty == nil {
		return nil, fmt.Errorf("a live shell needs a terminal")
	}
	script := `cd ` + quote(dir) + ` 2>/dev/null || ` +
		`{ echo "vibepod: ` + shellEscape(dir) + ` does not exist here; starting in $HOME" >&2; cd; }; ` +
		`exec "${SHELL:-/bin/sh}" -l`
	if h.enter != "" {
		// Inside that machine's pod, where the composed zone is at the same paths
		// as everywhere else. The cd therefore lands where it was asked to.
		script = `cd ` + quote(dir) + ` 2>/dev/null || cd; exec ` +
			strings.Replace(h.enter, " --", " --tty --", 1) + ` "${SHELL:-/bin/sh}" -l`
	}

	args := append(h.Opts(), "-tt", h.Alias, "sh -c "+quote(script))
	cmd := exec.Command("ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	// Its own session and controlling terminal, so job control on the far side
	// has something to talk to and a Ctrl-C reaches the remote foreground group
	// rather than the daemon.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	return cmd, nil
}

// shellEscape makes a path safe to put inside a double-quoted remote echo.
func shellEscape(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '"', '\\', '$', '`':
			out = append(out, '\\', r)
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
