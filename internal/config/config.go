// Package config reads vibepod.yaml and turns it into the two things the daemon
// actually needs: an ordered list of mounts, and the defaults a session starts
// with.
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
	// RemoteTools name commands that should not run in the pod: one that exists
	// only on another machine, or one that exists in both places and belongs on
	// the other. Each gets a three-line wrapper in /vp/bin, which leads PATH.
	RemoteTools RemoteTools `yaml:"remote_tools"`
	// CanMount is what `vp mount` may reach from inside the pod. Empty means
	// "any Host in your ssh config", which is the sane default: mounting opens a
	// network path out of a containment sandbox, so it is allowlisted, but the
	// allowlist you already maintain is your ssh config.
	CanMount []string `yaml:"can_mount"`
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
	// Default is the backend a new session opens on. It is not a guess about
	// anything: in v2 the machine is chosen, and this is the choice a session
	// starts with.
	Default string `yaml:"default"`
	// ForwardEnv says how much of a caller's environment a dispatched command
	// carries. Blanket forwarding is not an option: a pod's environment holds
	// the credentials that are the whole point of keeping the agent on one
	// machine, and describes this machine rather than the remote.
	//
	//	forward_env: delta          # default: what the caller set, only
	//	forward_env: none
	//	forward_env: [PYTHONPATH, CUDA_VISIBLE_DEVICES]
	ForwardEnv ForwardEnv `yaml:"forward_env"`
}

// ForwardEnv is a mode or an explicit list of names.
type ForwardEnv struct {
	Mode  string
	Names []string
}

// UnmarshalYAML accepts `delta`, `none`, or a list of variable names, because
// all three are things a person would reasonably write.
func (f *ForwardEnv) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var mode string
		if err := n.Decode(&mode); err != nil {
			return err
		}
		switch mode {
		case "delta", "none":
			f.Mode = mode
			return nil
		}
		return fmt.Errorf("forward_env: %q is not delta, none, or a list of "+
			"variable names", mode)
	case yaml.SequenceNode:
		if err := n.Decode(&f.Names); err != nil {
			return err
		}
		f.Mode = "explicit"
		return nil
	}
	return fmt.Errorf("forward_env: expected delta, none, or a list")
}

// RemoteTools is a list of names, or a machine-to-names map.
//
// The list form was enough while the working directory decided the machine. It
// is not any more: with the machine chosen per session, a wrapper invoked from a
// session that is in the pod has to be told where the tool lives, and guessing
// between two machines would be a guess about where work goes.
//
//	remote_tools: [rocm-smi, hipcc]        # one machine, or `vp @host` each time
//	remote_tools:
//	  gpu03: [rocm-smi, hipcc]
//	  build: [bazel]
type RemoteTools struct {
	Names []string
	Hosts map[string]string // tool -> machine
}

func (t *RemoteTools) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		return n.Decode(&t.Names)
	case yaml.MappingNode:
		var byHost map[string][]string
		if err := n.Decode(&byHost); err != nil {
			return err
		}
		t.Hosts = map[string]string{}
		hosts := make([]string, 0, len(byHost))
		for h := range byHost {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		for _, h := range hosts {
			for _, name := range byHost[h] {
				if prev, dup := t.Hosts[name]; dup {
					return fmt.Errorf("remote_tools: %s is claimed by both %s and %s",
						name, prev, h)
				}
				t.Hosts[name] = h
				t.Names = append(t.Names, name)
			}
		}
		return nil
	}
	return fmt.Errorf("remote_tools: expected a list of names, or a map of machine to names")
}

// Resolved is a config checked against the filesystem and flattened.
type Resolved struct {
	Name       string
	Tools      []string
	ToolHosts  map[string]string
	CanMount   []string
	ForwardEnv ForwardEnv
	// Mounts is ordered, and the order is kept all the way into the pod: it
	// decides what shadows what.
	Mounts  []proto.MountSpec
	Default string
}

// systemPaths may never be shadowed by a mount: a pod-local process reading
// /usr/lib would silently get another machine's files.
var systemPaths = []string{
	"/", "/usr", "/bin", "/lib", "/lib64", "/sbin", "/etc",
	"/proc", "/sys", "/dev", "/vp", "/var", "/run",
}

// GuardPath refuses a mountpoint that would shadow the system. It is exported
// because `vp mount` reaches it at runtime, an hour after `up`, and the same
// refusal has to hold then.
func GuardPath(at string) error {
	at = filepath.Clean(at)
	if !filepath.IsAbs(at) {
		return fmt.Errorf("%s is not an absolute path", at)
	}
	for _, s := range systemPaths {
		if at == s || isAncestor(at, s) {
			return fmt.Errorf("%s would mount over the system path %s; "+
				"give it an explicit `at:` if you meant it", at, s)
		}
	}
	return nil
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
	r := &Resolved{Name: c.Pod, Default: c.Exec.Default,
		Tools: c.RemoteTools.Names, ToolHosts: c.RemoteTools.Hosts,
		CanMount: c.CanMount, ForwardEnv: c.Exec.ForwardEnv}
	if r.ForwardEnv.Mode == "" {
		r.ForwardEnv.Mode = "delta"
	}
	if r.Name == "" {
		r.Name = "default"
	}
	if r.Default == "" {
		r.Default = route.Pod
	}
	claimed := map[string]string{}
	claim := func(at, by string) error {
		at = filepath.Clean(at)
		if err := GuardPath(at); err != nil {
			return fmt.Errorf("%s: %w", by, err)
		}
		if prev, dup := claimed[at]; dup {
			return fmt.Errorf("%s and %s both claim %s; set `at:` on one", prev, by, at)
		}
		claimed[at] = by
		return nil
	}
	origins := map[string]string{} // host:path already mounted

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
			if m.ExecOn != "" && !contains(m.ExposeTo, m.ExecOn) {
				return nil, fmt.Errorf("mount %s says it runs on %s but is not "+
					"exposed to it; add `expose_to: [%s]`", m.Local, m.ExecOn, m.ExecOn)
			}
			r.Mounts = append(r.Mounts, proto.MountSpec{At: at, Src: src,
				ReadOnly: m.ReadOnly, ExecOn: m.ExecOn})

		case m.Remote != "":
			host, path, ok := strings.Cut(m.Remote, ":")
			if !ok || host == "" || !strings.HasPrefix(path, "/") {
				return nil, fmt.Errorf("remote mount %q must be host:/absolute/path", m.Remote)
			}
			path = filepath.Clean(path)
			at := path
			if m.At != "" {
				at = m.At
			}
			if err := claim(at, m.Remote); err != nil {
				return nil, err
			}
			// Two caches over the same bytes cannot be made coherent
			// afterwards, so an overlap is refused rather than reported later.
			for prev, by := range origins {
				ph, pp, _ := strings.Cut(prev, ":")
				if ph != host {
					continue
				}
				if isUnder(path, pp) || isUnder(pp, path) {
					return nil, fmt.Errorf("%s overlaps %s; two caches over the "+
						"same files cannot be kept coherent, so mount the parent "+
						"once (%s)", m.Remote, by, prev)
				}
			}
			origins[host+":"+path] = m.Remote
			mode := m.Mode
			if mode == "" {
				mode = "fuse"
			}
			r.Mounts = append(r.Mounts, proto.MountSpec{At: at, Host: host,
				Path: path, ReadOnly: m.ReadOnly, Mode: mode, ExecOn: m.ExecOn})

		default:
			return nil, fmt.Errorf("a mount needs local: or remote:")
		}
	}

	// host_access is the identity plane: the agent's own config and credentials,
	// bound in and never transmitted anywhere.
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
		r.Mounts = append(r.Mounts, proto.MountSpec{At: src, Src: src, Identity: true})
	}
	// A tool named for a machine that this config never mentions is a typo, and
	// it is cheaper to say so now than when the wrapper is first invoked.
	known := map[string]bool{}
	for _, h := range c.HostList() {
		known[h] = true
	}
	for tool, host := range r.ToolHosts {
		if !known[host] {
			return nil, fmt.Errorf("remote_tools puts %s on %s, which this config "+
				"never mentions", tool, host)
		}
	}
	if r.Default != route.Pod && !known[r.Default] {
		return nil, fmt.Errorf("exec.default is %s, which this config never "+
			"mentions; mount something from it, or list it under hosts:", r.Default)
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
	for _, h := range c.RemoteTools.Hosts {
		add(h)
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

// isUnder reports whether path is inside prefix, or is it.
func isUnder(path, prefix string) bool {
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
