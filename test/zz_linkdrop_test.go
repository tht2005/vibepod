package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A dropped link must fail visibly and with a code of its own.
//
// Silently retrying a routed command is the one thing vibepod must never do:
// a `make deploy` or a migration that already ran halfway must not be run
// again behind the agent's back. A failure the agent can reason about beats a
// half-applied change it never learns about.
//
// The directory here is local, so the test is about the command's route and
// not about a mount that also died. (When a remote mount goes with the link,
// the shell's own `cd` fails first with EIO — vibepod does not shim shells,
// so that error is the shell's to report, not ours.)
func TestUnreachableHostFailsLoudlyWithItsOwnExitCode(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	// exec_on sends this directory's commands to a host that does not exist.
	cfg := "pod: e2e-dead\nmounts:\n  - local: " + work +
		"\n    expose_to: [nowhere-at-all]\n    exec_on: nowhere-at-all\n" +
		"exec:\n  default: pod\n"
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	defer vpctlIn(t, dir, "down", "e2e-dead")

	out, errOut, code := vpctlIn(t, dir, "run", "--", "/bin/sh", "-c",
		"cd "+work+" && /usr/bin/true")
	if code != 75 {
		t.Errorf("an unreachable host exited %d, want 75 (EX_TEMPFAIL); out=%q err=%q",
			code, out, errOut)
	}
	if !strings.Contains(errOut, "vibepod") {
		t.Errorf("the failure did not name vibepod, so it reads like the "+
			"command's own error: %q", errOut)
	}
}
