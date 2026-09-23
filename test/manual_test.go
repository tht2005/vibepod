package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"vibepod/internal/sys"
)

// TestManualConsole drives the console against a real config, for looking at
// rather than asserting on: terminal behaviour is easier to judge by reading a
// transcript than by matching strings.
//
//	VP_MANUAL=examples/gpu VP_POD=gpu VP_DIR=/some/remote/dir \
//	    go test ./test -run TestManualConsole -v
func TestManualConsole(t *testing.T) {
	project := os.Getenv("VP_MANUAL")
	if project == "" {
		t.Skip("set VP_MANUAL=<project dir> to drive a console against it")
	}
	abs, err := filepath.Abs(project)
	if err != nil {
		t.Fatal(err)
	}
	master, slave, err := sys.OpenPTY()
	if err != nil {
		t.Fatal(err)
	}
	_ = abs
	pod := os.Getenv("VP_POD")
	cmd := exec.Command(vpctlPath(t), "new", pod)
	cmd.Dir = abs
	cmd.Env = append(os.Environ(), "TERM=dumb")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	defer func() { cmd.Process.Kill(); cmd.Process.Wait(); master.Close() }()

	var out []byte
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				out = append(out, buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	time.Sleep(2 * time.Second)
	lines := []string{"/usr/bin/ls /\r"}
	if dir := os.Getenv("VP_DIR"); dir != "" {
		lines = append(lines, "cd "+dir+"\r", "/usr/bin/ls\r", "/usr/bin/uname -n\r")
	}
	for _, line := range lines {
		master.WriteString(line)
		time.Sleep(3500 * time.Millisecond)
	}
	t.Logf("\n%s", out)
}

// vpctlPath finds the built client next to the repository.
func vpctlPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../bin/vpctl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("build first: %v", err)
	}
	return p
}
