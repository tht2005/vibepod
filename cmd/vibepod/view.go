package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
)

// podArg resolves which pod a command means, in the order that surprises least:
// what you named, then the pod you are inside, then the one this project
// declares. An agent in a pod never has to know its own name, and a person in a
// project directory never has to type one.
func podArg(name string) string {
	if name != "" {
		return name
	}
	if p := os.Getenv("VIBEPOD_POD"); p != "" {
		return p
	}
	if m, err := loadSpec(""); err == nil {
		return m.Spec.Name
	}
	return ""
}

// cmdCd exists for the cockpit, which is the only caller that *can* change a
// working directory — its own. Anywhere else a process changing its caller's
// directory is impossible, so this says so and points at the composable form
// rather than failing silently.
func cmdCd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: vp cd @machine[/subdir]")
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	dir, target, err := machineDir(c, podArg(""), args[0])
	if err != nil {
		return err
	}
	return fmt.Errorf("cannot change the working directory of the shell that "+
		"called me.\n  In the cockpit, `cd %s` works.\n"+
		"  In a shell:  cd \"$(vp where -q %s)\"\n"+
		"  That is %s, on @%s", args[0], args[0], short(dir), target)
}

// cmdLog renders what has run and where. log and tree pair rather than
// overlap: log is flat, chronological and finished; tree is hierarchical,
// live and running.
func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	follow := fs.Bool("f", false, "keep printing as commands run")
	asJSON := fs.Bool("json", false, "NDJSON, one event per line")
	all := fs.Bool("all", false, "include the shells that wrapped these commands")
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
			if line := logLine(e, *follow, *all); line != "" {
				fmt.Println(line)
			}
		}
	}
}

// condense makes an agent's shell wrapper readable. Display only: the event
// stream and --json carry the line exactly as it was written, because that is
// what the log is for.
//
// The wrapper is real and measured — Claude Code runs every command as
// `zsh -c 'source <snapshot> && setopt … && eval <the command> && pwd -P >| …'`
// — and printing it whole fills the log with forty characters of prelude and
// none of the command. Each rule below removes one part of that shape and
// nothing else; anything it does not recognise is printed untouched.
func condense(line string) string {
	// The prelude: a chain of `… && …` whose early links are the environment
	// snapshot and shell options.
	for {
		head, rest, ok := strings.Cut(line, " && ")
		if !ok {
			break
		}
		h := strings.TrimSpace(head)
		if strings.HasPrefix(h, "source ") || strings.HasPrefix(h, ". ") ||
			strings.HasPrefix(h, "setopt ") || strings.HasPrefix(h, "shopt ") ||
			strings.HasPrefix(h, "unsetopt ") {
			line = rest
			continue
		}
		break
	}
	// The cwd capture at the end, which is how the agent tracks `cd` between
	// calls — bookkeeping, not a command.
	for _, sep := range []string{" && pwd -P", " ; pwd -P", "; pwd -P"} {
		if i := strings.Index(line, sep); i > 0 {
			line = line[:i]
		}
	}
	line = strings.TrimSpace(line)
	// `eval '<the command>'`, which is the command itself with one layer of
	// quoting around it.
	if rest, ok := strings.CutPrefix(line, "eval "); ok {
		rest = strings.TrimSpace(rest)
		if len(rest) > 1 && (rest[0] == '\'' || rest[0] == '"') &&
			rest[len(rest)-1] == rest[0] {
			line = strings.ReplaceAll(rest[1:len(rest)-1], "'\\''", "'")
		}
	}
	return strings.TrimSpace(line)
}

func logLine(e event.Event, follow, all bool) string {
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
			trim(condense(strings.Join(e.Argv, " ")), 40), mark, dur(e.ElapsedMS))
	case event.KindExec:
		if !follow {
			return "" // unfinished work belongs to the tree
		}
		return fmt.Sprintf("%s  %-8s %-40s ●", ts, e.Target,
			trim(condense(strings.Join(e.Argv, " ")), 40))
	case event.KindPod:
		return fmt.Sprintf("%s  %-8s pod %s %s", ts, "-", e.Pod, e.Detail)
	case event.KindSession:
		if !follow {
			return ""
		}
		return fmt.Sprintf("%s  %-8s session %s %s", ts, orDash(e.Target),
			e.Session, e.Detail)
	case event.KindNotice:
		return fmt.Sprintf("%s  %-8s %-40s %s", ts, e.Target,
			trim(condense(strings.Join(e.Argv, " ")), 40), e.Detail)
	}
	return ""
}

// cmdHosts answers "where can I go", which in a pod is a question about
// machines and not about paths.
func cmdHosts(args []string) error {
	fs := flag.NewFlagSet("hosts", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	reply, err := call(c, &proto.Msg{Op: proto.OpHosts, Pod: podArg(fs.Arg(0))})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(reply.Hosts)
	}
	fmt.Print(renderHosts(reply.Hosts))
	return nil
}

// renderHosts is shared with the console, which prints the same list for a
// bare `cd`: the question "where can I go" has one answer wherever it is asked.
func renderHosts(hosts []proto.HostInfo) string {
	var b strings.Builder
	// As wide as the longest name: ssh aliases like aiotlab_3gpus_aiotlab are
	// ordinary, and a fixed column turns the list into a mess.
	w := 6
	for _, h := range hosts {
		if len(h.Name) > w {
			w = len(h.Name)
		}
	}
	for _, h := range hosts {
		status := "connected"
		switch {
		case h.Local:
			status = "here"
		case !h.Mounted:
			// A machine in your ssh config that this pod has not mounted. It is
			// listed because `vp mount` can reach it, which is the answer to
			// "what else is there".
			status = "not mounted"
		case !h.Connected:
			status = "not connected"
		}
		if h.Default {
			status += " · default"
		}
		dir := "(no directory of its own)"
		switch {
		case len(h.Dirs) > 0:
			dir = short(h.Dirs[0])
		case !h.Mounted:
			dir = "vp mount " + h.Name + ":/path"
		}
		fmt.Fprintf(&b, "  @%-*s  %-19s %s\n", w, h.Name, status, dir)
		// A machine this pod has not mounted owns no directories at all, which
		// the old code could not represent: it always had at least one.
		if len(h.Dirs) > 1 {
			for _, extra := range h.Dirs[1:] {
				fmt.Fprintf(&b, "   %-*s  %-19s %s\n", w, "", "", short(extra))
			}
		}
	}
	return b.String()
}

// cmdWhere answers both readings of the word, because both are asked.
//
//	vpctl where           which machine does this directory run on
//	vpctl where @gpu03    which directory does that machine own
//
// It prints a bare path with -q, so a real shell can compose with it:
//
//	cd "$(vpctl where -q @gpu03)"
//
// That composition is the reason this is a separate command from `cd`. A
// process cannot change its caller's working directory, so anything that
// claims to is either lying or is the caller itself.
func cmdWhere(args []string) error {
	fs := flag.NewFlagSet("where", flag.ContinueOnError)
	quiet := fs.Bool("q", false, "print only the path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	pod := podArg("")

	if arg := fs.Arg(0); arg != "" {
		dir, target, err := machineDir(c, pod, arg)
		if err != nil {
			return err
		}
		if *quiet {
			fmt.Println(dir)
			return nil
		}
		fmt.Printf("%s is @%s\n", short(dir), target)
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	reply, err := call(c, &proto.Msg{Op: proto.OpStat, Pod: pod, Path: cwd,
		Session: os.Getenv("VIBEPOD_SESSION")})
	if err != nil {
		return err
	}
	if *quiet {
		fmt.Println(reply.Backend)
		return nil
	}
	// Two facts, deliberately separate, because v1 conflated them and got both
	// wrong: whose files these are, and where a command here would run.
	owner := "this machine"
	if reply.Target != "pod" {
		owner = "@" + reply.Target
	}
	if reply.Backend == "pod" {
		fmt.Printf("%s belongs to %s; commands from this session run here, in the pod\n",
			short(cwd), owner)
	} else {
		fmt.Printf("%s belongs to %s; commands from this session run on @%s, where it is %s\n",
			short(cwd), owner, reply.Backend, reply.Path)
	}
	if reply.Detail != "" && reply.Detail != reply.Backend {
		fmt.Printf("  this directory suggests @%s — `vp use %s`\n",
			reply.Detail, reply.Detail)
	}
	return nil
}

// machineDir resolves "@gpu03" to the directory that machine owns.
func machineDir(c *proto.Conn, pod, arg string) (dir, target string, err error) {
	name, sub, _ := strings.Cut(strings.TrimPrefix(arg, "@"), "/")
	if name == "local" {
		name = "pod"
	}
	reply, err := call(c, &proto.Msg{Op: proto.OpHosts, Pod: pod})
	if err != nil {
		return "", "", err
	}
	for _, h := range reply.Hosts {
		if h.Name != name {
			continue
		}
		switch len(h.Dirs) {
		case 0:
			return "", "", fmt.Errorf("@%s has no directory of its own in this pod", name)
		case 1:
			return filepath.Join(h.Dirs[0], sub), h.Name, nil
		default:
			// Naming a machine cannot pick between its directories, and
			// guessing would be worse than asking.
			var b strings.Builder
			fmt.Fprintf(&b, "@%s owns more than one directory; name one:\n", name)
			for _, d := range h.Dirs {
				fmt.Fprintf(&b, "  %s\n", short(d))
			}
			return "", "", fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
		}
	}
	var known []string
	for _, h := range reply.Hosts {
		known = append(known, "@"+h.Name)
	}
	return "", "", fmt.Errorf("no machine @%s in this pod; there is %s",
		name, strings.Join(known, ", "))
}

// cmdTree is the one view nothing else can produce: pstree stops at the
// machine boundary, and here the machine is a column.
func cmdTree(args []string) error {
	fs := flag.NewFlagSet("tree", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	all := fs.Bool("all", false, "include completed commands instead of a count")
	follow := fs.Bool("f", false, "stream changes as NDJSON instead of a snapshot")
	expand := fs.Bool("x", false, "ask each machine what is running under its commands")
	running := fs.Bool("running", false, "only what is still running")
	failed := fs.Bool("failed", false, "only what exited non-zero")
	since := fs.Duration("since", 0, "only what started within this long, e.g. 10m")
	mountsOnly := fs.Bool("mounts", false, "only the mounts half")
	execOnly := fs.Bool("exec", false, "only the sessions half")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if *follow {
		// The tree and the log are two renderings of one event stream, so
		// following either is the same act: subscribe to it.
		return streamEvents(fs.Arg(0))
	}
	// `vp tree 412` is one subtree; `vp tree work` is one pod. A number is a pid,
	// because a pod called 412 is not a thing anyone does.
	podName, root := "", 0
	for _, a := range fs.Args() {
		if n, err := strconv.Atoi(a); err == nil {
			root = n
		} else {
			podName = a
		}
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	m := &proto.Msg{Op: proto.OpTree, Pod: podArg(podName),
		// What broke recently is in the history, which is only sent when asked.
		All: *all || *failed || *since > 0}
	if *expand {
		m.Detail = "expand"
	}
	reply, err := call(c, m)
	if err != nil {
		return err
	}
	t := reply.Tree
	if t == nil {
		return fmt.Errorf("no tree returned")
	}
	keep := treeFilter{running: *running, failed: *failed, root: root}
	if *since > 0 {
		keep.after = time.Now().Add(-*since).UnixMilli()
	}
	if keep.active() {
		t = keep.apply(t)
	}
	if *mountsOnly {
		t.Sessions = nil
	}
	if *execOnly {
		t.Mounts = nil
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(t)
	}
	renderTree(t, m.All, !*execOnly, !*mountsOnly)
	return nil
}

// reorder puts flags before operands, so `vp tree work --running` works as well as
// `vp tree --running work`. Go's flag package stops at the first operand.
func reorder(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if a == "--since" || a == "-since" {
				if i+1 < len(args) {
					i++
					flags = append(flags, args[i])
				}
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

// treeFilter is the views of §9 that are questions rather than layouts: what is
// still running, what broke recently, one subtree. Done here, over the tree the
// daemon already sends, because they are renderings of one data source.
type treeFilter struct {
	running bool
	failed  bool
	after   int64
	root    int
}

func (f treeFilter) active() bool {
	return f.running || f.failed || f.after > 0 || f.root > 0
}

func (f treeFilter) match(n proto.TreeNode) bool {
	if f.running && n.State != "running" {
		return false
	}
	if f.failed && (n.Code == nil || *n.Code == 0) {
		return false
	}
	if f.after > 0 && n.StartedMS < f.after {
		return false
	}
	return true
}

// prune keeps a node if it matches or anything under it does, so the answer still
// shows *where* a failing command ran — its parents are the context.
func (f treeFilter) prune(n proto.TreeNode) (proto.TreeNode, bool) {
	var kids []proto.TreeNode
	for _, c := range n.Children {
		if k, ok := f.prune(c); ok {
			kids = append(kids, k)
		}
	}
	n.Children = kids
	return n, f.match(n) || len(kids) > 0
}

func find(nodes []proto.TreeNode, pid int) *proto.TreeNode {
	for i := range nodes {
		if nodes[i].PID == pid {
			return &nodes[i]
		}
		if n := find(nodes[i].Children, pid); n != nil {
			return n
		}
	}
	return nil
}

func (f treeFilter) apply(t *proto.Tree) *proto.Tree {
	out := *t
	out.Sessions = nil
	out.Completed = 0
	for _, s := range t.Sessions {
		nodes := s.Nodes
		if f.root > 0 {
			n := find(nodes, f.root)
			if n == nil {
				continue
			}
			nodes = []proto.TreeNode{*n}
		}
		var kept []proto.TreeNode
		for _, n := range nodes {
			if k, ok := f.prune(n); ok {
				kept = append(kept, k)
			}
		}
		if len(kept) > 0 {
			s.Nodes = kept
			out.Sessions = append(out.Sessions, s)
		}
	}
	return &out
}

// streamEvents is the daemon's own event stream, one object per line. The
// moment an agent parses this it is an API, which is what the "v" field is
// for: it will outlive several rounds of the tree's visual layout.
func streamEvents(pod string) error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Send(&proto.Msg{Op: proto.OpLog, Pod: podArg(pod), Follow: true}); err != nil {
		return err
	}
	for {
		m, _, err := c.Recv()
		if err != nil {
			return nil
		}
		switch m.Op {
		case proto.OpErr:
			return fmt.Errorf("%s", m.Err)
		case proto.OpEvent:
			fmt.Printf("%s\n", m.Event)
		}
	}
}

func renderTree(t *proto.Tree, all, showMounts, showExec bool) {
	fmt.Printf("%s · running %s · new sessions open on %s\n\n", t.Pod, t.Uptime,
		t.Default)
	if showMounts {
		renderMounts(t)
	}
	if showExec {
		renderSessions(t, all)
	}
}

func renderMounts(t *proto.Tree) {
	fmt.Println("mounts")
	for i, m := range t.Mounts {
		fmt.Printf("%s %-28s ← %-34s %-6s %s\n", branch(i, len(t.Mounts)),
			trim(short(m.At), 28), trim(short(m.Source), 34), m.Kind, rw(m.ReadOnly))
		// A suggestion, printed as one: it is what the directory says it is for,
		// not where anything actually goes.
		if m.ExecOn != "" {
			fmt.Printf("%s    meant for → %s\n", cont(i, len(t.Mounts)), m.ExecOn)
		}
	}
	fmt.Println()
}

func renderSessions(t *proto.Tree, all bool) {
	fmt.Println("sessions")
	if len(t.Sessions) == 0 {
		fmt.Println("  (none)")
	}
	for i, s := range t.Sessions {
		label := fmt.Sprintf("session %s", s.ID)
		if s.Backend != "" {
			label += "  on " + s.Backend
		}
		if s.Kind != "" {
			label += "  (" + s.Kind + ")"
		}
		fmt.Printf("%s %s\n", branch(i, len(t.Sessions)), label)
		prefix := "│  "
		if i == len(t.Sessions)-1 {
			prefix = "   "
		}
		for j, n := range s.Nodes {
			renderNode(n, prefix, j == len(s.Nodes)-1)
		}
	}
	if !all && t.Completed > 0 {
		fmt.Printf("\n(%d completed — `vp tree --all` to expand)\n", t.Completed)
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
		trim(condense(strings.Join(n.Argv, " ")), 40), n.Target, dur(n.ElapsedMS),
		mark)
	// What `-x` found on the far side, marked as polled: vibepod saw it by asking
	// `ps`, not by starting it, and the difference is worth keeping visible.
	for i, r := range n.Remote {
		t := "├┄"
		if i == len(n.Remote)-1 && len(n.Children) == 0 {
			t = "└┄"
		}
		fmt.Printf("%s%s %-40s %-8s %s\n", next, t, trim(r.Args, 40), n.Target,
			"(polled)")
	}
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

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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

// trim shortens a string to n printable columns, leaving escape sequences whole:
// cutting one in half leaves the terminal in whatever mode it named.
func trim(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if !strings.ContainsRune(s, 0x1b) && len(s) <= n {
		return s
	}
	var b strings.Builder
	count, esc := 0, false
	for _, r := range s {
		if esc {
			b.WriteRune(r)
			if r == 'm' || r == 'K' || r == 'J' || r == 'H' {
				esc = false
			}
			continue
		}
		if r == 0x1b {
			esc = true
			b.WriteRune(r)
			continue
		}
		if count == n-1 && visibleLen(s) > n {
			b.WriteString("…")
			// Close anything that was opened, so the rest of the line is not
			// painted in whatever colour we cut away from.
			if strings.ContainsRune(s, 0x1b) {
				b.WriteString(reset)
			}
			return b.String()
		}
		if count >= n {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

const (
	dim   = "\x1b[2m"
	bold  = "\x1b[1m"
	reset = "\x1b[0m"
)

// visibleLen counts printable width, so an escape sequence does not eat a
// column.
func visibleLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc && (r == 'm' || r == 'K' || r == 'J' || r == 'H'):
			esc = false
		case esc:
		case r == 0x1b:
			esc = true
		default:
			n++
		}
	}
	return n
}
