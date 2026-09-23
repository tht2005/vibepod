package e2e

import (
	"strings"
	"testing"
	"time"
)

// M7: the tree's views. One data source, several questions: what is still
// running, what broke recently, one subtree, one half.

func TestTreeRunningShowsOnlyWhatIsStillRunning(t *testing.T) {
	requireSSH(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		inRemotePod(t, "vp @vptest /bin/sh -c 'sleep 3'")
	}()
	defer func() { <-done }()
	// Ended before the snapshot, so it must not appear.
	inRemotePod(t, "vp @vptest /usr/bin/true")
	time.Sleep(700 * time.Millisecond)

	out, _, code := vpIn(t, remoteDir, "tree", "--running", "e2e-remote")
	if code != 0 {
		t.Fatalf("tree --running: %s", out)
	}
	if !strings.Contains(out, "sleep 3") {
		t.Errorf("the running command is missing:\n%s", out)
	}
	if strings.Contains(out, "/usr/bin/true") {
		t.Errorf("a finished command is shown as running:\n%s", out)
	}
}

// -x asks the machine what is running under a dispatched command. Polled, and
// marked so: vibepod saw it by asking, not by starting it.
func TestTreeExpandShowsWhatARemoteCommandSpawned(t *testing.T) {
	requireSSH(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		inRemotePod(t, "vp @vptest /bin/sh -c 'sleep 4; true'")
	}()
	defer func() { <-done }()
	time.Sleep(1 * time.Second)

	out, _, code := vpIn(t, remoteDir, "tree", "-x", "--running", "e2e-remote")
	if code != 0 {
		t.Fatalf("tree -x: %s", out)
	}
	if !strings.Contains(out, "sleep 4") || !strings.Contains(out, "(polled)") {
		t.Errorf("-x did not show the remote child, marked as polled:\n%s", out)
	}
	// And without -x it is a leaf: nothing is claimed that was not asked for.
	out, _, _ = vpIn(t, remoteDir, "tree", "--running", "e2e-remote")
	if strings.Contains(out, "(polled)") {
		t.Errorf("remote children appeared without -x:\n%s", out)
	}
}

func TestTreeFailedSinceFindsWhatBrokeRecently(t *testing.T) {
	requireSSH(t)
	inRemotePod(t, "vp @vptest /bin/sh -c 'exit 7'")
	inRemotePod(t, "vp @vptest /usr/bin/true")
	time.Sleep(300 * time.Millisecond)
	out, _, code := vpIn(t, remoteDir, "tree", "--failed", "--since", "1m", "e2e-remote")
	if code != 0 {
		t.Fatalf("tree --failed: %s", out)
	}
	if !strings.Contains(out, "exit 7") || !strings.Contains(out, "✗ 7") {
		t.Errorf("the failed command is missing:\n%s", out)
	}
	if strings.Contains(out, "/usr/bin/true") {
		t.Errorf("a successful command is listed as failed:\n%s", out)
	}
}

func TestTreeHalves(t *testing.T) {
	inPod(t, "true")
	mounts, _, _ := vp(t, "tree", "--mounts", "e2e")
	if !strings.Contains(mounts, "\nmounts\n") || strings.Contains(mounts, "\nsessions\n") {
		t.Errorf("--mounts did not show only the mounts:\n%s", mounts)
	}
	execs, _, _ := vp(t, "tree", "--exec", "e2e")
	if strings.Contains(execs, "\nmounts\n") || !strings.Contains(execs, "\nsessions\n") {
		t.Errorf("--exec did not show only the sessions:\n%s", execs)
	}
}
