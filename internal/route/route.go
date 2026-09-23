// Package route decides which machine a command runs on.
//
// The rule is "cwd decides", so an agent inherits routing without being told
// anything: it runs commands where its files are, which is what it would do on
// a normal machine.
package route

import (
	"path/filepath"
	"sort"
	"strings"
)

// Pod is the target meaning "run here, in the pod".
const Pod = "pod"

// Rule maps a directory subtree to the machine that owns it.
type Rule struct {
	Prefix string // pod-absolute path
	Target string // Pod, or an ssh_config host alias
}

// Table is the resolved route set for one pod.
type Table struct {
	rules   []Rule // longest prefix first
	Default string
}

// New sorts rules so that the most specific match wins, which is what makes
// an "at:" override of a nested directory behave the way a reader expects.
func New(def string, rules []Rule) *Table {
	if def == "" {
		def = Pod
	}
	sorted := append([]Rule(nil), rules...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return len(sorted[i].Prefix) > len(sorted[j].Prefix)
	})
	return &Table{rules: sorted, Default: def}
}

// Route resolves a working directory to a machine, in the precedence order of
// DESIGN.md §5: a session pin beats the directory, the directory beats the
// default.
func Route(t *Table, cwd, pin string) string {
	// The pod's own machinery always runs locally, whatever the pin says:
	// MCP servers and agent helpers must never be shipped to a remote.
	if under(cwd, "/vp") {
		return Pod
	}
	if pin != "" && pin != "auto" {
		return pin
	}
	if t == nil {
		return Pod
	}
	for _, r := range t.rules {
		if under(cwd, r.Prefix) {
			return r.Target
		}
	}
	return t.Default
}

// Rules exposes the table for rendering (vpctl tree).
func (t *Table) Rules() []Rule { return t.rules }

func under(path, prefix string) bool {
	path = filepath.Clean(path)
	prefix = filepath.Clean(prefix)
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/")
}
