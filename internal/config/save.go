package config

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// `vp save`: the config is a snapshot you take, not a file you restart for.
//
// The pod is the live object. Everything in vibepod.yaml can change while it
// runs, so the file's job is to be a starting point and, afterwards, a record of
// where you got to. This renders the live state back into it.

// Snapshot is the live state, in the shape the file wants it.
type Snapshot struct {
	Pod      string
	Default  string
	Path     string // where it came from, so save knows where to write
	Mounts   []SnapMount
	Tools    []SnapTool
	CanMount []string
}

type SnapMount struct {
	Local    bool
	Host     string
	Path     string
	Src      string
	At       string
	ReadOnly bool
	ExecOn   string
	Identity bool
}

type SnapTool struct {
	Name string
	Host string
}

// Render turns the live state into vibepod.yaml.
//
// It writes the file it would have read: every field round-trips, so `vp save`
// followed by `vibepod up` reproduces the pod that was running. Comments in the
// original are not preserved, which is why save prints the result and asks
// rather than overwriting — see the client.
func Render(s *Snapshot) (string, error) {
	c := Config{Pod: s.Pod}
	if s.Default != "" {
		c.Exec.Default = s.Default
	}
	for _, m := range s.Mounts {
		if m.Identity {
			c.HostAccess = append(c.HostAccess, shortenHome(m.Src))
			continue
		}
		out := Mount{At: m.At, ReadOnly: m.ReadOnly, ExecOn: m.ExecOn}
		if m.Local {
			out.Local = shortenHome(m.Src)
			if out.At == out.Local {
				out.At = ""
			}
		} else {
			out.Remote = m.Host + ":" + m.Path
			if m.At == m.Path {
				out.At = ""
			}
		}
		c.Mounts = append(c.Mounts, out)
	}
	byHost := map[string][]string{}
	for _, t := range s.Tools {
		byHost[t.Host] = append(byHost[t.Host], t.Name)
	}
	// Always the map form on the way out: it says which machine each tool is on,
	// which is the thing a reader of the file most needs and the list form cannot
	// express.
	if len(byHost) > 0 {
		c.RemoteTools = RemoteTools{Hosts: map[string]string{}}
		for host, names := range byHost {
			for _, n := range names {
				c.RemoteTools.Hosts[n] = host
			}
		}
	}
	c.CanMount = s.CanMount

	var b strings.Builder
	b.WriteString("# Written by `vp save`: the pod as it was actually running.\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(renderView(&c, byHost)); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// renderView is the config as a plain map, so the mount list keeps its order and
// empty fields stay out of the file. The struct tags alone would emit every zero
// value, and a config full of `at: ""` is worse than no config.
func renderView(c *Config, tools map[string][]string) map[string]any {
	out := map[string]any{"pod": c.Pod}
	var mounts []map[string]any
	for _, m := range c.Mounts {
		one := map[string]any{}
		if m.Local != "" {
			one["local"] = m.Local
		}
		if m.Remote != "" {
			one["remote"] = m.Remote
		}
		if m.At != "" {
			one["at"] = m.At
		}
		if m.ReadOnly {
			one["readonly"] = true
		}
		if m.ExecOn != "" {
			one["exec_on"] = m.ExecOn
		}
		mounts = append(mounts, one)
	}
	if len(mounts) > 0 {
		out["mounts"] = mounts
	}
	if len(tools) > 0 {
		out["remote_tools"] = tools
	}
	if len(c.HostAccess) > 0 {
		out["host_access"] = c.HostAccess
	}
	if len(c.CanMount) > 0 {
		out["can_mount"] = c.CanMount
	}
	if c.Exec.Default != "" {
		out["exec"] = map[string]any{"default": c.Exec.Default}
	}
	return out
}

// shortenHome writes ~ back, because that is what the file said in the first
// place and a saved config full of /home/you is not shareable.
func shortenHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(p, home+"/") {
		return p
	}
	return "~" + strings.TrimPrefix(p, home)
}
