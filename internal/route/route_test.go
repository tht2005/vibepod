package route

import "testing"

func TestRoutePrecedence(t *testing.T) {
	tbl := New(Pod, []Rule{
		{Prefix: "/srv", Target: "prod"},
		{Prefix: "/srv/api/vendor", Target: Pod},
		{Prefix: "/home/u/proj", Target: "gpu-box"},
	})
	cases := []struct{ cwd, pin, want string }{
		{"/srv/api", "", "prod"},
		{"/srv/api/vendor/x", "", Pod},      // most specific rule wins
		{"/home/u/proj/src", "", "gpu-box"}, // exec_on, not the mount owner
		{"/elsewhere", "", Pod},             // falls back to the default
		{"/srv/api", "gpu-box", "gpu-box"},  // a session pin overrides the directory
		{"/vp/real/x", "gpu-box", Pod},      // pod machinery is never shipped out
		{"/srvvv", "", Pod},                 // prefix match is path-aware
	}
	for _, c := range cases {
		if got := Route(tbl, c.cwd, c.pin); got != c.want {
			t.Errorf("Route(%q, pin=%q) = %q, want %q", c.cwd, c.pin, got, c.want)
		}
	}
}
