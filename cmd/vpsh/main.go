// vpsh is the pod's $SHELL.
//
// It records the command line it was asked to run and then runs it, by exec'ing
// the user's real shell in place. That is the whole program: about sixty lines,
// no kernel help, nothing to unmount, and it cannot get the command wrong
// because it does not interpret it.
//
// It replaced a seccomp gate on execve. The gate saw more — every exec, not only
// the ones that came through a shell — and it could redirect them, which is what
// it was built for. DESIGN.md §3 records why that was removed anyway: a
// redirected execve is not a process, it is an ssh, and the list of things it
// silently loses does not terminate.
//
// What vpsh deliberately does *not* do is ship the command somewhere else. A
// non-interactive shell always runs in the pod, because the wrapper an agent
// generates is written in this machine's shell syntax, sources an environment
// snapshot from this machine's disk, and records its new working directory to a
// temp file on this machine. Shipping that string to a remote breaks all three,
// and the third one breaks silently. Dispatch is one level down — `vp @host cmd`,
// or a wrapper in /vp/bin — and an interactive session on another machine is not
// vpsh at all: it is that machine's own shell, with the terminal wired to it.
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"vibepod/internal/pod"
	"vibepod/internal/proto"
)

// exitUnavailable is EX_TEMPFAIL: vibepod could not run this command at all.
// Distinct from anything the command itself would return.
const exitUnavailable = 75

func main() {
	shell := realShell()
	// Record first, then run. If anything about recording fails — no daemon, a
	// wedged socket, a missing variable — the command still runs. A pod whose
	// commands stop working because the log is unavailable would be a worse
	// trade than an incomplete log.
	report(os.Args[1:])

	argv := append([]string{shell}, os.Args[1:]...)
	if err := syscall.Exec(shell, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "vibepod: cannot run %s: %v\n", shell, err)
		os.Exit(exitUnavailable)
	}
}

// realShell is the shell the pod's user actually has. The daemon puts it in the
// environment; the fallbacks exist for a shell started outside a session.
func realShell() string {
	for _, cand := range []string{os.Getenv("VIBEPOD_REAL_SHELL"), os.Getenv("SHELL")} {
		if cand == "" || strings.HasPrefix(cand, pod.VpDir+"/") {
			continue // not ourselves, and not anything else under /vp
		}
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return "/bin/sh"
}

// report tells the daemon what is about to run here.
//
// It waits for the acknowledgement, briefly. The wait is what makes the log
// ordered — two commands in a pipeline reach the daemon in the order the shell
// started them — and the bound is what makes it safe: a daemon that has stopped
// answering costs each command a second, not the pod.
func report(args []string) {
	sock := os.Getenv("VIBEPOD_SOCK")
	if sock == "" {
		sock = pod.SockPath
	}
	c, err := proto.Dial(sock)
	if err != nil {
		return
	}
	defer c.Close()
	cwd, _ := os.Getwd()
	if err := c.Send(&proto.Msg{
		Op:      proto.OpExec,
		Argv:    commandLine(args),
		Cwd:     cwd,
		Session: os.Getenv("VIBEPOD_SESSION"),
	}); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _, _ = c.Recv()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// commandLine is what to record: the script for a `-c`, and the shell's own
// arguments otherwise.
//
// The script is recorded whole, exactly as it was written, because this log is
// the trust surface and an abbreviated record of what ran on another machine is
// worth very little. Making it readable is the reader's job — see condense() in
// the client, which is display only.
func commandLine(args []string) []string {
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			return []string{args[i+1]}
		}
	}
	if len(args) == 0 {
		return []string{"(interactive shell)"}
	}
	return args
}
