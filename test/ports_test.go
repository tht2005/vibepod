package e2e

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A port on another machine, reachable here as localhost — and gone when the pod
// is, rather than outliving it as a stray `ssh -L`.
func TestPortsForwardAndCloseWithThePod(t *testing.T) {
	requireSSH(t)
	// Something listening "on the remote": the fixture host is this machine, so a
	// listener here is on its loopback.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fmt.Fprint(c, "hello from the remote")
			c.Close()
		}
	}()
	remote := ln.Addr().(*net.TCPAddr).Port
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	local := free.Addr().(*net.TCPAddr).Port
	free.Close()

	dir := t.TempDir()
	cfg := fmt.Sprintf("pod: e2e-ports\nmounts:\n  - local: %s\nports:\n  - vptest:%d:%d\n",
		workDir, remote, local)
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := vpIn(t, dir, "up"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if got := dial(local); got != "hello from the remote" {
		t.Fatalf("the forward did not reach the remote port: %q", got)
	}
	out, _, _ := vpIn(t, dir, "forward")
	if !strings.Contains(out, fmt.Sprintf("localhost:%d", local)) {
		t.Errorf("`vp forward` does not list the open forward:\n%s", out)
	}
	vpIn(t, dir, "down", "e2e-ports")
	time.Sleep(300 * time.Millisecond)
	if got := dial(local); got != "" {
		t.Errorf("the forward outlived the pod: %q", got)
	}
}

func dial(port int) string {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	return string(b)
}
