package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mode: sync — a local copy kept the same around execution rather than on a timer.
func TestSyncModeKeepsTwoCopiesTheSameAroundExecution(t *testing.T) {
	requireSSH(t)
	remote := t.TempDir() // "on vptest": the fixture host is this machine
	if err := os.WriteFile(filepath.Join(remote, "start.txt"), []byte("was there\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "delete-me-here.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := "pod: e2e-sync\nmounts:\n  - remote: vptest:" + remote + "\n    mode: sync\n"
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	defer vpIn(t, dir, "down", "e2e-sync")
	_, errOut, code := vpIn(t, dir, "up")
	if code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if !strings.Contains(errOut, "copying vptest") {
		t.Errorf("up did not say it was copying: %s", errOut)
	}
	inSync := func(script string) (string, string, int) {
		return vpIn(t, dir, "run", "--", "/bin/sh", "-c", "cd "+remote+" && "+script)
	}

	// Pulled at up: the pod reads the copy.
	if out, _, _ := inSync("/usr/bin/cat start.txt"); !strings.Contains(out, "was there") {
		t.Fatalf("the copy was not pulled at up: %q", out)
	}
	// It is a local copy, not the remote: writing here does not touch the remote...
	inSync("echo edited-here > local.txt && rm delete-me-here.txt")
	if _, err := os.Stat(filepath.Join(remote, "local.txt")); err == nil {
		t.Fatal("a local write reached the remote without a command running there")
	}
	// ...until a command runs there, which sees it, and sees the deletion.
	out, _, _ := inSync("vp @vptest /bin/sh -c 'cat local.txt; ls'")
	if !strings.Contains(out, "edited-here") {
		t.Errorf("a local edit was not pushed before the command: %q", out)
	}
	if strings.Contains(out, "delete-me-here") {
		t.Errorf("a local deletion was not pushed: %q", out)
	}
	// What the command wrote there is here afterwards.
	inSync("vp @vptest /bin/sh -c 'echo made-there > remote.txt; rm start.txt'")
	if out, _, _ := inSync("/usr/bin/cat remote.txt"); !strings.Contains(out, "made-there") {
		t.Errorf("a remote write was not pulled after the command: %q", out)
	}
	if _, _, code := inSync("/usr/bin/test -e start.txt"); code == 0 {
		t.Errorf("a remote deletion was not pulled")
	}

	// A conflict is kept, not guessed at: changed here, deleted there.
	inSync("vp @vptest /usr/bin/true") // settle
	time.Sleep(1100 * time.Millisecond)
	inSync("echo changed-here >> remote.txt")
	if err := os.Remove(filepath.Join(remote, "remote.txt")); err != nil {
		t.Fatal(err)
	}
	inSync("vp @vptest /usr/bin/true")
	if out, _, _ := inSync("/usr/bin/cat remote.txt"); !strings.Contains(out, "changed-here") {
		t.Errorf("a file changed here and deleted there was lost: %q", out)
	}
	log, _, _ := vpIn(t, dir, "log", "e2e-sync")
	if !strings.Contains(log, "kept remote.txt") {
		t.Errorf("the conflict was not reported:\n%s", log)
	}

	// Deleted over there outside any command, and unchanged here: it stays deleted,
	// rather than being sent back by the next push.
	inSync("echo short-lived > oob.txt && vp @vptest /usr/bin/true")
	time.Sleep(1100 * time.Millisecond)
	if err := os.Remove(filepath.Join(remote, "oob.txt")); err != nil {
		t.Fatal(err)
	}
	inSync("vp @vptest /usr/bin/true")
	if _, err := os.Stat(filepath.Join(remote, "oob.txt")); err == nil {
		t.Errorf("a file deleted on the remote was sent back by the next push")
	}
	if _, _, code := inSync("/usr/bin/test -e oob.txt"); code == 0 {
		t.Errorf("a file deleted on the remote is still in the local copy")
	}

	// And `down` pushes what is left, since the copy here is about to go.
	inSync("echo last-words > final.txt")
	vpIn(t, dir, "down", "e2e-sync")
	if b, err := os.ReadFile(filepath.Join(remote, "final.txt")); err != nil ||
		!strings.Contains(string(b), "last-words") {
		t.Errorf("down did not push the last edits: %v %q", err, b)
	}
}
