package main

import (
	"fmt"
	"os"
	"os/exec"
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
func call(c *proto.Conn, m *proto.Msg, fds ...int) (*proto.Msg, error) {
	if err := c.Send(m, fds...); err != nil {
		return nil, err
	}
	reply, _, err := c.Recv()
	if err != nil {
		return nil, err
	}
	if reply.Op == proto.OpErr {
		return nil, fmt.Errorf("%s", reply.Err)
	}
	return reply, nil
}
