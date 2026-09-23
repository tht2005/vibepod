// Package config reads vibepod.yaml and turns it into the two things the
// daemon actually needs: a set of mounts, and a routing table.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vibepod/internal/proto"
	"vibepod/internal/route"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Pod        string          `yaml:"pod"`
	Agents     []string        `yaml:"agents"`
	Hosts      map[string]Host `yaml:"hosts"`
	Mounts     []Mount         `yaml:"mounts"`
	HostAccess []string        `yaml:"host_access"`
	Ports      []string        `yaml:"ports"`
	Exec       Exec            `yaml:"exec"`
	// RemoteTools name commands that exist only on a remote. Without an entry
	// here there is nothing in the pod to shim, so `rocm-smi` in a routed
	// directory fails as "not found" rather than being sent to the machine
	// that has it.
	RemoteTools []string `yaml:"remote_tools"`
}

type Host struct {
	Toolbin            bool `yaml:"toolbin"`
	ForwardCredentials bool `yaml:"forward_credentials"`
}

type Mount struct {
	Local    string   `yaml:"local"`
	Remote   string   `yaml:"remote"` // host:/path
	At       string   `yaml:"at"`
	Mode     string   `yaml:"mode"`
	ReadOnly bool     `yaml:"readonly"`
	ExecOn   string   `yaml:"exec_on"`
	ExposeTo []string `yaml:"expose_to"`
}

type Exec struct {
	Default string `yaml:"default"`
}

// Resolved is a config checked against the filesystem and flattened.
type Resolved struct {
	Name        string
	RemoteTools []string
	Binds       []proto.Bind
	Remotes     []RemoteMount
	Routes      []proto.Route
	ExecDefault string
}

// RemoteMount is a directory on another machine that the daemon must mount on
// the host and bind into the pod.
type RemoteMount struct {
	Host     string
	Path     string // path on the remote
	At       string // path in the pod
	ReadOnly bool
	Mode     string
}

// systemPaths may never be shadowed by a mount: a pod-local process reading
// /usr/lib would silently get another machine's files.
var systemPaths = []string{
	"/", "/usr", "/bin", "/lib", "/lib64", "/sbin", "/etc",
	"/proc", "/sys", "/dev", "/vp", "/var", "/run",
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Find walks up from dir looking for a project config.
func Find(dir string) (string, bool) {
	for {
		for _, name := range []string{"vibepod.local.yaml", "vibepod.yaml"} {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err == nil {
				return p, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// Resolve expands paths and applies the placement guard. Every refusal happens
// here, at up time, while a human is watching — never mid-run.
func (c *Config) Resolve() (*Resolved, error) {
	r := &Resolved{Name: c.Pod, ExecDefault: c.Exec.Default, RemoteTools: c.RemoteTools}
	if r.Name == "" {
		r.Name = "default"
	}
	if r.ExecDefault == "" {
		r.ExecDefault = route.Pod
	}
	claimed := map[string]string{}
	claim := func(at, by string) error {
		at = filepath.Clean(at)
		for _, s := range systemPaths {
			if at == s || isAncestor(at, s) {
				return fmt.Errorf("%s would mount over the system path %s; "+
					"give it an explicit `at:` if you meant it", by, s)
			}
		}
		if prev, dup := claimed[at]; dup {
			return fmt.Errorf("%s and %s both claim %s; set `at:` on one", prev, by, at)
		}
		claimed[at] = by
		return nil
	}

	for _, m := range c.Mounts {
		switch {
		case m.Local != "" && m.Remote != "":
			return nil, fmt.Errorf("mount %q sets both local: and remote:", m.Local)

		case m.Local != "":
			src, err := expand(m.Local)
			if err != nil {
				return nil, err
			}
			if _, err := os.Stat(src); err != nil {
				return nil, fmt.Errorf("local mount %s: %w", m.Local, err)
			}
			at := src
			if m.At != "" {
				at = m.At
			}
			if err := claim(at, m.Local); err != nil {
				return nil, err
			}
			r.Binds = append(r.Binds, proto.Bind{Src: src, Dst: at, ReadOnly: m.ReadOnly})
			if m.ExecOn != "" {
				if !contains(m.ExposeTo, m.ExecOn) {
					return nil, fmt.Errorf("mount %s runs on %s but is not exposed to it; "+
						"add `expose_to: [%s]`", m.Local, m.ExecOn, m.ExecOn)
				}
				r.Routes = append(r.Routes, proto.Route{Prefix: at, Target: m.ExecOn})
			} else {
				r.Routes = append(r.Routes, proto.Route{Prefix: at, Target: route.Pod})
			}

		case m.Remote != "":
			host, path, ok := strings.Cut(m.Remote, ":")
			if !ok || host == "" || !strings.HasPrefix(path, "/") {
				return nil, fmt.Errorf("remote mount %q must be host:/absolute/path", m.Remote)
			}
			at := path
			if m.At != "" {
				at = m.At
			}
			if err := claim(at, m.Remote); err != nil {
				return nil, err
			}
			target := host
			if m.ExecOn != "" {
				target = m.ExecOn
			}
			mode := m.Mode
			if mode == "" {
				mode = "fuse"
			}
			r.Remotes = append(r.Remotes, RemoteMount{Host: host, Path: path,
				At: at, ReadOnly: m.ReadOnly, Mode: mode})
			r.Routes = append(r.Routes, proto.Route{Prefix: at, Target: target,
				RemotePrefix: path})

		default:
			return nil, fmt.Errorf("a mount needs local: or remote:")
		}
	}

	// host_access is the identity plane: the agent's own config and
	// credentials, bound in and never transmitted anywhere.
	for _, p := range c.HostAccess {
		src, err := expand(p)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(src); err != nil {
			continue // not every agent is installed
		}
		if err := claim(src, p); err != nil {
			return nil, err
		}
		r.Binds = append(r.Binds, proto.Bind{Src: src, Dst: src})
	}
	return r, nil
}

// HostList is every machine this config refers to, however it refers to it.
func (c *Config) HostList() []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		if h == "" || h == route.Pod || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	for h := range c.Hosts {
		add(h)
	}
	for _, m := range c.Mounts {
		if host, _, ok := strings.Cut(m.Remote, ":"); ok {
			add(host)
		}
		add(m.ExecOn)
		for _, h := range m.ExposeTo {
			add(h)
		}
	}
	add(c.Exec.Default)
	sort.Strings(out)
	return out
}

func expand(p string) (string, error) {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		p = abs
	}
	return filepath.Clean(p), nil
}

// isAncestor reports whether at sits above sys, which would shadow it.
func isAncestor(at, sys string) bool {
	return at != "/" && strings.HasPrefix(sys, strings.TrimSuffix(at, "/")+"/")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
