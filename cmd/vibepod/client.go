package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"vibepod/internal/daemon"
	"vibepod/internal/proto"
)

// connect reaches the daemon, starting it if this is the first use. The
// daemon is a user process with no privileges, so starting it needs no
// ceremony and asks nothing of the user.
func connect() (*proto.Conn, error) {
	// Inside a pod, the only socket that exists is the pod's own — which is
	// deliberately the weaker of the two. There is no host socket to find.
	if s := os.Getenv("VIBEPOD_SOCK"); s != "" {
		return proto.Dial(s)
	}
	sock := daemon.HostSock()
	if c, err := proto.Dial(sock); err == nil {
		warnStaleDaemon()
		return c, nil
	}
	if err := spawnDaemon(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := proto.Dial(sock); err == nil {
			return c, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon did not come up; see %s/daemon.log", daemon.RunDir())
}

// dialQuiet reaches a daemon that is already running, and says nothing: it is
// for requests made from inside an interface that owns the terminal.
func dialQuiet() (*proto.Conn, error) {
	if s := os.Getenv("VIBEPOD_SOCK"); s != "" {
		return proto.Dial(s)
	}
	return proto.Dial(daemon.HostSock())
}

// warnStaleDaemon says so when the daemon is from a different build than this
// binary. It is not an error — the old daemon works fine, for the old
// behaviour — but the symptom otherwise is a change you just made having no
// effect at all, which is a bad hour.
func warnStaleDaemon() {
	b, err := os.ReadFile(filepath.Join(daemon.RunDir(), "version"))
	if err != nil {
		return
	}
	if running := strings.TrimSpace(string(b)); running != daemon.Version {
		fmt.Fprintf(os.Stderr,
			"vibepod: the running daemon is build %s, this is %s. "+
				"`vibepod down` your pods and stop it (pkill -f 'vibepod daemon') "+
				"to pick up the new one.\n",
			running, daemon.Version)
	}
}

func spawnDaemon() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	return cmd.Process.Release()
}

// call sends one request and returns the reply, turning a protocol error into
// a Go error.
//
// A request may report progress before it answers. Creating a pod can mean
// waiting on several machines, each with ten seconds to prove it exists, and a
// wait that says nothing is indistinguishable from a hang.
func call(c *proto.Conn, m *proto.Msg, fds ...int) (*proto.Msg, error) {
	if err := c.Send(m, fds...); err != nil {
		return nil, err
	}
	partial := false
	for {
		reply, _, err := c.Recv()
		if err != nil {
			if partial {
				fmt.Fprintln(os.Stderr)
			}
			return nil, err
		}
		if reply.Op == proto.OpProgress {
			if !partial {
				fmt.Fprint(os.Stderr, "vibepod: ")
			}
			fmt.Fprint(os.Stderr, reply.Detail)
			if !reply.Partial {
				fmt.Fprintln(os.Stderr)
			}
			partial = reply.Partial
			continue
		}
		if partial {
			// The step that was in progress never got its outcome.
			fmt.Fprintln(os.Stderr)
		}
		if reply.Op == proto.OpErr {
			return nil, fmt.Errorf("%s", reply.Err)
		}
		return reply, nil
	}
}
