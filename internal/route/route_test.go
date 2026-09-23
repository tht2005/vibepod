package route

import "testing"

func TestDirNamesADirectoryOnTheChosenMachine(t *testing.T) {
	tbl := New(Pod, []Rule{
		{Prefix: "/srv/api", Owner: "prod", RemotePath: "/srv/api"},
		{Prefix: "/staging-api", Owner: "staging", RemotePath: "/srv/api"},
	})
	// Path identity: the directory means the same thing on both machines, so
	// there is nothing to translate and nothing that can go wrong.
	if d := Dir(tbl, "/srv/api/src", "prod"); d != "/srv/api/src" {
		t.Errorf("identity mount rewrote the path: %q", d)
	}
	// An explicit at: is the one case that needs naming back.
	if d := Dir(tbl, "/staging-api/src", "staging"); d != "/srv/api/src" {
		t.Errorf("at: override named %q on staging", d)
	}
	// A machine that owns none of this path gets it unchanged, and finds out
	// for itself whether it has one — which fails visibly rather than wrongly.
	if d := Dir(tbl, "/staging-api/src", "gpu03"); d != "/staging-api/src" {
		t.Errorf("a path was rewritten for a machine that owns no part of it: %q", d)
	}
	if d := Dir(tbl, "/srv/api/src", Pod); d != "/srv/api/src" {
		t.Errorf("the pod's own path was rewritten: %q", d)
	}
}

func TestOwnerIsTheMostSpecificMount(t *testing.T) {
	tbl := New(Pod, []Rule{
		{Prefix: "/srv", Owner: "prod"},
		{Prefix: "/srv/api/vendor", Owner: Pod},
		{Prefix: "/home/u/proj", Owner: Pod, ExecOn: "gpu-box"},
	})
	cases := []struct{ cwd, want string }{
		{"/srv/api", "prod"},
		{"/srv/api/vendor/x", Pod}, // the most specific rule wins
		{"/home/u/proj/src", Pod},  // local files, whatever exec_on suggests
		{"/elsewhere", Pod},
		{"/srvvv", Pod}, // prefix matching is path-aware
	}
	for _, c := range cases {
		if got := Owner(tbl, c.cwd); got != c.want {
			t.Errorf("Owner(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
	// exec_on is a suggestion and says so; the mount's owner is the fallback
	// suggestion for a remote directory.
	if s := Suggest(tbl, "/home/u/proj/src"); s != "gpu-box" {
		t.Errorf("exec_on was not surfaced as a suggestion: %q", s)
	}
	if s := Suggest(tbl, "/srv/api"); s != "prod" {
		t.Errorf("a remote mount should suggest its owner, got %q", s)
	}
	if s := Suggest(tbl, "/srv/api/vendor/x"); s != "" {
		t.Errorf("a local subtree suggested %q", s)
	}
}

func TestPodMachineryIsPrivate(t *testing.T) {
	for _, p := range []string{"/vp", "/vp/bin/vp", "/vp/run/pod.sock"} {
		if !Private(p) {
			t.Errorf("%s should be private to the pod", p)
		}
	}
	if Private("/vpsomething") {
		t.Error("prefix matching is not path-aware")
	}
}
