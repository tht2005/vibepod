package config

import "testing"

func TestParseRemoteTakesAnOptionalPodPath(t *testing.T) {
	cases := []struct{ in, host, path, at string }{
		{"gpu03:/data/imagenet", "gpu03", "/data/imagenet", ""},
		{"gpu03:/data/imagenet:/datasets", "gpu03", "/data/imagenet", "/datasets"},
		{"gpu03:/data/imagenet/", "gpu03", "/data/imagenet", ""},
		// A colon inside the remote path is fine once the pod path is given: the
		// pod path is what follows the last ":/".
		{"gpu03:/odd:name/x:/p", "gpu03", "/odd:name/x", "/p"},
	}
	for _, c := range cases {
		h, p, at, err := ParseRemote(c.in)
		if err != nil || h != c.host || p != c.path || at != c.at {
			t.Errorf("ParseRemote(%q) = %q %q %q %v, want %q %q %q", c.in, h, p, at,
				err, c.host, c.path, c.at)
		}
	}
	for _, bad := range []string{"gpu03", "gpu03:relative", ":/x", ""} {
		if _, _, _, err := ParseRemote(bad); err == nil {
			t.Errorf("ParseRemote(%q) accepted a malformed mount", bad)
		}
	}
}
