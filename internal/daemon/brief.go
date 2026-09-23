package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"vibepod/internal/route"
)

// The agent brief.
//
// In v1 an agent inherited its routing whether it knew about vibepod or not, so
// telling it was a nicety that never got built. In v2 it is the mechanism: with
// no exec gate, an agent that has not been told about `vp` will run everything
// in the pod, over FUSE, on the wrong machine — and be slow and confused about
// why the GPUs are missing.
//
// It is written as a project instruction file, in the agent's own language, and
// linked from the root of the pod's filesystem. No MCP server, no new tool to
// learn, and it is regenerated whenever the mounts change.

// brief renders the pod's current shape as instructions.
func (s *podState) brief() string {
	tbl := s.table()
	var b strings.Builder
	b.WriteString("# vibepod\n\n")
	fmt.Fprintf(&b, "You are in a vibepod called `%s`: a namespace on one machine that also\n", s.name)
	b.WriteString("holds directories belonging to others.\n\n")
	b.WriteString("**Commands run on this session's backend, which is chosen rather than guessed.**\n")
	fmt.Fprintf(&b, "New sessions start on `%s`. A command you type runs there unless you say otherwise,\n", tbl.Default)
	b.WriteString("so a compile or a test in a mounted directory does *not* reach that machine on its own.\n\n")

	b.WriteString("## Where things are\n\n```\n")
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MACHINE\tDIRECTORY\tNOTE")
	local, remote := 0, 0
	for _, r := range tbl.Rules() {
		note := ""
		switch {
		case r.Owner == route.Pod && r.ExecOn != "":
			note = "local files, meant to run on " + r.ExecOn
			local++
		case r.Owner == route.Pod:
			note = "local files"
			local++
		default:
			note = "network filesystem — see below"
			remote++
		}
		owner := r.Owner
		if owner == route.Pod {
			owner = "pod"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", owner, r.Prefix, note)
	}
	_ = w.Flush()
	b.WriteString("```\n\n")
	b.WriteString("A directory has the same absolute path on every machine that has it, so a path\n")
	b.WriteString("in an error message or a traceback can be opened from anywhere.\n\n")

	b.WriteString("## How to run something elsewhere\n\n```sh\n")
	b.WriteString("vp backend                 # which machine is this session on?\n")
	b.WriteString("vp hosts                   # what machines does this pod know?\n")
	machine := "gpu03"
	if ms := s.machines(); len(ms) > 0 {
		machine = ms[0]
	}
	fmt.Fprintf(&b, "vp @%s <cmd>...          # run one command there; the session does not move\n", machine)
	fmt.Fprintf(&b, "vp use %s               # move this session; later commands go there\n", machine)
	b.WriteString("vp tree                    # what is running, and where\n")
	b.WriteString("```\n\n")

	if remote > 0 {
		b.WriteString("## Two things worth knowing\n\n")
		b.WriteString("1. A mounted directory is reachable here, but over the network. Reading a file\n")
		b.WriteString("   is fine; searching a large tree, building, or running tests is far faster on\n")
		fmt.Fprintf(&b, "   the machine that owns it — `vp @%s rg pattern .` rather than `rg` here.\n", machine)
		b.WriteString("2. A tool that only exists on another machine already works by name: it has a\n")
		b.WriteString("   wrapper in /vp/bin, which leads PATH, and the wrapper dispatches it.\n\n")
	}
	s.mu.Lock()
	tools := make([]string, 0, len(s.toolHosts))
	for name := range s.toolHosts {
		tools = append(tools, name)
	}
	s.mu.Unlock()
	if len(tools) > 0 {
		fmt.Fprintf(&b, "Dispatched by name, wherever you run them from: %s.\n\n",
			strings.Join(tools, ", "))
	}
	b.WriteString("Your own configuration and credentials are bound in from the machine you are on\n")
	b.WriteString("and are never sent anywhere else. Nothing you run needs to be installed on a\n")
	b.WriteString("remote for any of this to work.\n")
	return b.String()
}

// refreshBrief rewrites the brief in place. It lives in the pod's runtime
// directory on the host, which is bound in at /vp/run, so the daemon can update
// it without asking vpinit for anything — and an agent that reads it after a
// `vp mount` sees the machine that was just added.
func (s *podState) refreshBrief() {
	path := filepath.Join(s.runDir(), "ctl", "brief.md")
	if err := os.WriteFile(path, []byte(s.brief()), 0o644); err != nil {
		s.d.logf("pod %s: rewrite the agent brief: %v", s.name, err)
	}
}
