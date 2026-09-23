package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A dropped link must fail visibly and with a code of its own.
//
// Silently retrying a dispatched command is the one thing vibepod must never do:
// a `make deploy` or a migration that already ran halfway must not be run again
// behind the agent's back. A failure the agent can reason about beats a
// half-applied change it never learns about.
func TestUnreachableHostFailsLoudlyWithItsOwnExitCode(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	// A machine that does not exist, named as a backend rather than inferred:
	// `hosts:` is enough to make it a machine this pod knows about, and the
	// failure then happens where the command is sent rather than at `up`.
	cfg := "pod: e2e-dead\nhosts:\n  nowhere-at-all: {}\nmounts:\n  - local: " +
		work + "\nexec:\n  default: pod\n"
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	defer vpIn(t, dir, "down", "e2e-dead")

	out, errOut, code := vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"cd "+work+" && vp @nowhere-at-all /usr/bin/true")
	if code != 75 {
		t.Errorf("an unreachable host exited %d, want 75 (EX_TEMPFAIL); out=%q err=%q",
			code, out, errOut)
	}
	if !strings.Contains(errOut, "vibepod") {
		t.Errorf("the failure did not name vibepod, so it reads like the "+
			"command's own error: %q", errOut)
	}
}
