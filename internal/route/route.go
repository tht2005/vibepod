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
	// RemotePrefix is where that subtree lives on Target. Under path identity
	// it equals Prefix, and only an explicit `at:` makes them differ.
	RemotePrefix string
}

// Decision is a resolved route: the machine, and the directory to run in as
// that machine sees it.
type Decision struct {
	Target string
	Dir    string
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

// Resolve maps a working directory to a machine and the directory to use
// there, in the precedence order of DESIGN.md §5: a session pin beats the
// directory, the directory beats the default.
func Resolve(t *Table, cwd, pin string) Decision {
	// The pod's own machinery always runs locally, whatever the pin says:
	// MCP servers and agent helpers must never be shipped to a remote.
	if under(cwd, "/vp") {
		return Decision{Target: Pod, Dir: cwd}
	}
	if pin != "" && pin != "auto" {
		// A pin says nothing about paths, so the directory is passed through
		// unchanged: it works when the target can see it, and fails visibly
		// when it cannot.
		return Decision{Target: pin, Dir: translate(t, cwd, pin)}
	}
	if t == nil {
		return Decision{Target: Pod, Dir: cwd}
	}
	for _, r := range t.rules {
		if under(cwd, r.Prefix) {
			return Decision{Target: r.Target, Dir: rebase(cwd, r.Prefix, r.RemotePrefix)}
		}
	}
	return Decision{Target: t.Default, Dir: cwd}
}

// Route is the target alone, for callers that do not need a directory.
func Route(t *Table, cwd, pin string) string { return Resolve(t, cwd, pin).Target }

// translate finds a rule that both contains cwd and belongs to the pinned
// host, so an explicit `at:` still maps correctly under a pin.
func translate(t *Table, cwd, pin string) string {
	if t == nil {
		return cwd
	}
	for _, r := range t.rules {
		if r.Target == pin && under(cwd, r.Prefix) {
			return rebase(cwd, r.Prefix, r.RemotePrefix)
		}
	}
	return cwd
}

func rebase(cwd, prefix, remotePrefix string) string {
	if remotePrefix == "" || remotePrefix == prefix {
		return cwd
	}
	rest := strings.TrimPrefix(filepath.Clean(cwd), filepath.Clean(prefix))
	return filepath.Join(remotePrefix, rest)
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
