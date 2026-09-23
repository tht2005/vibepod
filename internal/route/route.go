// Package route answers one question: what is this directory called on that
// machine?
//
// It used to answer two, and the other one was wrong. v1 inferred the machine
// from the working directory, through a seccomp exec gate; DESIGN.md §3 records
// why that was removed. The machine is now chosen — it is the session's
// backend — so all that is left here is the path map.
package route

import (
	"path/filepath"
	"sort"
	"strings"
)

// Pod is the backend meaning "run here, in the pod".
const Pod = "pod"

// Rule maps a directory subtree in the pod to the machine whose filesystem it
// is, and to what that machine calls it.
type Rule struct {
	Prefix string // pod-absolute path
	Owner  string // Pod, or the ssh alias whose disk this is
	// RemotePath is what Owner calls this subtree. Under path identity it
	// equals Prefix, and only an explicit `at:` makes them differ.
	RemotePath string
	// ExecOn is a suggestion, not a rule: "the machine this directory is
	// meant to run on". §5 — it is surfaced by `vp hosts`, the tree and the
	// generated agent brief, and it never moves a command on its own.
	ExecOn string
}

// Table is the resolved mount set for one pod.
type Table struct {
	rules   []Rule // longest prefix first
	Default string // the backend a new session opens on
}

// New sorts rules so that the most specific match wins, which is what makes an
// `at:` override of a nested directory behave the way a reader expects.
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

// Dir is what cwd is called on backend.
//
// Under path identity the answer is cwd itself, and that is the answer in every
// case except an explicit `at:` — a mount deliberately given a different name
// in the pod than it has at home. Then the two differ, and only the machine
// that owns the subtree knows the other name.
func Dir(t *Table, cwd, backend string) string {
	if t == nil || backend == Pod {
		return cwd
	}
	for _, r := range t.rules {
		if r.Owner == backend && under(cwd, r.Prefix) {
			return rebase(cwd, r.Prefix, r.RemotePath)
		}
	}
	return cwd
}

// Owner is the machine whose filesystem this directory is, which is not the
// same question as where a command in it runs.
func Owner(t *Table, cwd string) string {
	if t == nil {
		return Pod
	}
	for _, r := range t.rules {
		if under(cwd, r.Prefix) {
			return r.Owner
		}
	}
	return Pod
}

// Suggest is the backend this directory says it would rather run on, or "".
func Suggest(t *Table, cwd string) string {
	if t == nil {
		return ""
	}
	for _, r := range t.rules {
		if under(cwd, r.Prefix) {
			if r.ExecOn != "" {
				return r.ExecOn
			}
			if r.Owner != Pod {
				return r.Owner
			}
			return ""
		}
	}
	return ""
}

// Private reports whether a path is the pod's own machinery, which never
// crosses to another machine: the socket, the wrappers, the agent's brief.
func Private(path string) bool { return under(path, "/vp") }

func rebase(cwd, prefix, remotePath string) string {
	if remotePath == "" || remotePath == prefix {
		return cwd
	}
	rest := strings.TrimPrefix(filepath.Clean(cwd), filepath.Clean(prefix))
	return filepath.Join(remotePath, rest)
}

// Rules exposes the table for rendering (vp tree, vp hosts).
func (t *Table) Rules() []Rule { return t.rules }

func under(path, prefix string) bool {
	path = filepath.Clean(path)
	prefix = filepath.Clean(prefix)
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/")
}
