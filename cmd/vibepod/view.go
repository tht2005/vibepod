package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
)

// podArg falls back to the pod this process is running in, so an agent
// inside a pod never has to know its own name.
func podArg(name string) string {
	if name != "" {
		return name
	}
	return os.Getenv("VIBEPOD_POD")
}

// cmdLog renders what has run and where. log and tree pair rather than
// overlap: log is flat, chronological and finished; tree is hierarchical,
// live and running.
func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	follow := fs.Bool("f", false, "keep printing as commands run")
	asJSON := fs.Bool("json", false, "NDJSON, one event per line")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Send(&proto.Msg{Op: proto.OpLog, Pod: podArg(fs.Arg(0)),
		Follow: *follow}); err != nil {
		return err
	}
	for {
		m, _, err := c.Recv()
		if err != nil {
			return nil
		}
		switch m.Op {
		case proto.OpEnd:
			return nil
		case proto.OpErr:
			return fmt.Errorf("%s", m.Err)
		case proto.OpEvent:
			if *asJSON {
				fmt.Printf("%s\n", m.Event)
				continue
			}
			var e event.Event
			if err := json.Unmarshal(m.Event, &e); err != nil {
				continue
			}
			if line := logLine(e, *follow); line != "" {
				fmt.Println(line)
			}
		}
	}
}

func logLine(e event.Event, follow bool) string {
	ts := "        "
	if t, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
		ts = t.Format("15:04:05")
	}
	switch e.Kind {
	case event.KindExit:
		// vibepod is the parent of routed commands but only an observer of
		// pod-local ones, so a status is often genuinely unknown. Printing a
		// tick for those would invent a fact.
		mark := " "
		if e.Code != nil {
			mark = "✓"
			if *e.Code != 0 {
				mark = fmt.Sprintf("✗ %d", *e.Code)
			}
		}
		return fmt.Sprintf("%s  %-8s %-40s %s %s", ts, e.Target,
			trim(strings.Join(e.Argv, " "), 40), mark, dur(e.ElapsedMS))
	case event.KindExec:
		if !follow {
			return "" // unfinished work belongs to the tree
		}
		return fmt.Sprintf("%s  %-8s %-40s ●", ts, e.Target,
			trim(strings.Join(e.Argv, " "), 40))
	case event.KindPod:
		return fmt.Sprintf("%s  %-8s pod %s %s", ts, "-", e.Pod, e.Detail)
	}
	return ""
}

// cmdTree is the one view nothing else can produce: pstree stops at the
// machine boundary, and here the machine is a column.
func cmdTree(args []string) error {
	fs := flag.NewFlagSet("tree", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	all := fs.Bool("all", false, "include completed commands instead of a count")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	reply, err := call(c, &proto.Msg{Op: proto.OpTree, Pod: podArg(fs.Arg(0)), All: *all})
	if err != nil {
		return err
	}
	t := reply.Tree
	if t == nil {
		return fmt.Errorf("no tree returned")
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(t)
	}
	renderTree(t, *all)
	return nil
}

func renderTree(t *proto.Tree, all bool) {
	fmt.Printf("%s · running %s · exec: %s\n\n", t.Pod, t.Uptime, t.ExecDefault)

	fmt.Println("mounts")
	for i, m := range t.Mounts {
		fmt.Printf("%s %-28s ← %-34s %-6s %s\n", branch(i, len(t.Mounts)),
			trim(short(m.At), 28), trim(short(m.Source), 34), m.Kind, rw(m.ReadOnly))
		if m.Target != "" && m.Target != "pod" {
			fmt.Printf("%s    exec_on → %s\n", cont(i, len(t.Mounts)), m.Target)
		}
	}
	fmt.Println("\nexec")
	if len(t.Sessions) == 0 {
		fmt.Println("  (nothing running)")
	}
	for i, s := range t.Sessions {
		fmt.Printf("%s session %s\n", branch(i, len(t.Sessions)), s.ID)
		prefix := "│  "
		if i == len(t.Sessions)-1 {
			prefix = "   "
		}
		for j, n := range s.Nodes {
			renderNode(n, prefix, j == len(s.Nodes)-1)
		}
	}
	if !all && t.Completed > 0 {
		fmt.Printf("\n(%d completed — vpctl tree --all to expand)\n", t.Completed)
	}
}

func renderNode(n proto.TreeNode, prefix string, last bool) {
	tee := "├─"
	next := prefix + "│  "
	if last {
		tee = "└─"
		next = prefix + "   "
	}
	mark := ""
	if n.State == "running" {
		mark = "●"
	} else if n.Code != nil && *n.Code != 0 {
		mark = fmt.Sprintf("✗ %d", *n.Code)
	}
	fmt.Printf("%s%s %-40s %-8s %7s %s\n", prefix, tee,
		trim(strings.Join(n.Argv, " "), 40), n.Target, dur(n.ElapsedMS), mark)
	for i, c := range n.Children {
		renderNode(c, next, i == len(n.Children)-1)
	}
}

func branch(i, n int) string {
	if i == n-1 {
		return "└─"
	}
	return "├─"
}

func cont(i, n int) string {
	if i == n-1 {
		return "  "
	}
	return "│ "
}

func rw(ro bool) string {
	if ro {
		return "ro"
	}
	return "rw"
}

func dur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return d.Truncate(time.Second).String()
	}
}

// short replaces the home directory with ~, which is most of what makes a
// mount table readable.
func short(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
