package config

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// The ssh config is vibepod's host list, and its mount allowlist.
//
// Hosts are ssh_config aliases throughout, so ProxyJump, keys, ports and
// forwarding are inherited rather than reimplemented. That has a second use: the
// allowlist `vp mount` is checked against is one the user already maintains. A
// machine in your ssh config is one you have already decided to talk to.

var (
	sshOnce  sync.Once
	sshNames []string
)

// SSHHosts lists the Host aliases in the user's ssh config, skipping patterns —
// a `Host *` entry is settings, not a machine.
func SSHHosts() []string {
	sshOnce.Do(func() { sshNames = readSSHHosts(sshConfigPaths()) })
	return sshNames
}

// InSSHConfig reports whether a name is one of those aliases.
func InSSHConfig(name string) bool {
	for _, h := range SSHHosts() {
		if h == name {
			return true
		}
	}
	return false
}

func sshConfigPaths() []string {
	if f := os.Getenv("VIBEPOD_SSH_CONFIG"); f != "" {
		return []string{f}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".ssh", "config")}
}

// readSSHHosts parses Host lines, following Include as ssh itself does — a
// config split across files is common enough that ignoring it would make the
// allowlist wrong rather than merely incomplete.
func readSSHHosts(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	var visit func(path string, depth int)
	visit = func(path string, depth int) {
		if depth > 8 {
			return
		}
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, rest, ok := cutField(line)
			if !ok {
				continue
			}
			switch strings.ToLower(key) {
			case "host":
				for _, name := range strings.Fields(rest) {
					// A pattern is settings for many machines, not a machine.
					if strings.ContainsAny(name, "*?!") || seen[name] {
						continue
					}
					seen[name] = true
					out = append(out, name)
				}
			case "include":
				for _, pat := range strings.Fields(rest) {
					for _, inc := range expandInclude(pat, filepath.Dir(path)) {
						visit(inc, depth+1)
					}
				}
			}
		}
	}
	for _, p := range paths {
		visit(p, 0)
	}
	sort.Strings(out)
	return out
}

func expandInclude(pat, dir string) []string {
	if strings.HasPrefix(pat, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			pat = filepath.Join(home, strings.TrimPrefix(pat, "~/"))
		}
	}
	if !filepath.IsAbs(pat) {
		pat = filepath.Join(dir, pat)
	}
	matches, err := filepath.Glob(pat)
	if err != nil {
		return nil
	}
	return matches
}

// cutField splits "Key value" on whitespace or an "=" separator, both of which
// ssh accepts.
func cutField(line string) (key, rest string, ok bool) {
	if i := strings.IndexAny(line, " \t="); i > 0 {
		return line[:i], strings.TrimLeft(line[i+1:], " \t="), true
	}
	return "", "", false
}
