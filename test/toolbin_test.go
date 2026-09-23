package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// toolbin: a tool this machine has and a remote does not, copied over when the
// config allows it — and only then, and only if it would run there.

func staticTool(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(`package main
import ("fmt"; "os")
func main() { fmt.Println("static tool says", os.Args[1:]) }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, "vp-static-tool"), src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	t.Cleanup(func() {
		sshCapture(t, "vptest", "rm -f ~/.vp/bin/vp-static-tool")
	})
	return bin
}

func toolbinProject(t *testing.T, name string, allow bool) string {
	t.Helper()
	dir := t.TempDir()
	flag := "{}"
	if allow {
		flag = "{toolbin: true}"
	}
	cfg := "pod: " + name + "\nhosts:\n  vptest: " + flag + "\nmounts:\n  - local: " +
		workDir + "\nexec:\n  default: pod\n"
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAMissingStaticToolIsCopiedWhereAllowed(t *testing.T) {
	requireSSH(t)
	bin := staticTool(t)
	sshCapture(t, "vptest", "rm -f ~/.vp/bin/vp-static-tool")
	d := startDaemon(t, "PATH="+bin+":"+os.Getenv("PATH"))
	dir := toolbinProject(t, "e2e-toolbin", true)
	defer d.vp(dir, "down", "e2e-toolbin")
	if _, errOut, code := d.vp(dir, "up"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	out, errOut, code := d.vp(dir, "run", "--", "/bin/sh", "-c", "vp @vptest vp-static-tool hi")
	if code != 0 || !strings.Contains(out, "static tool says [hi]") {
		t.Fatalf("the tool was not copied and run (exit %d): %q %q", code, out, errOut)
	}
	log, _, _ := d.vp(dir, "log", "e2e-toolbin")
	if !strings.Contains(log, "copied vp-static-tool") {
		t.Errorf("the copy was not reported:\n%s", log)
	}
}

func TestAMissingToolIsNotCopiedWithoutPermission(t *testing.T) {
	requireSSH(t)
	bin := staticTool(t)
	sshCapture(t, "vptest", "rm -f ~/.vp/bin/vp-static-tool")
	d := startDaemon(t, "PATH="+bin+":"+os.Getenv("PATH"))
	dir := toolbinProject(t, "e2e-notoolbin", false)
	defer d.vp(dir, "down", "e2e-notoolbin")
	if _, errOut, code := d.vp(dir, "up"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	_, _, code := d.vp(dir, "run", "--", "/bin/sh", "-c", "vp @vptest vp-static-tool hi")
	if code != 127 {
		t.Errorf("exit %d, want 127: the tool is not there and must not be copied", code)
	}
	if out, _ := sshCapture(t, "vptest", "test -e ~/.vp/bin/vp-static-tool && echo copied || echo absent"); strings.TrimSpace(out) != "absent" {
		t.Errorf("the tool was copied without permission")
	}
	log, _, _ := d.vp(dir, "log", "e2e-notoolbin")
	if !strings.Contains(log, "toolbin: true") {
		t.Errorf("the log does not say how to allow it:\n%s", log)
	}
}
