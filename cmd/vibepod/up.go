package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"vibepod/internal/config"
	"vibepod/internal/proto"
	"vibepod/internal/term"
)

// ensureUp creates the pod if it is not already running.
func ensureUp(c *proto.Conn, m *proto.Msg) error {
	if _, err := call(c, m); err != nil &&
		!strings.Contains(err.Error(), "already running") {
		return err
	}
	return nil
}

// loadSpec turns the project config into an up request.
func loadSpec(name string) (*proto.Msg, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	path, found := config.Find(cwd)
	if !found {
		return nil, fmt.Errorf("no vibepod.yaml here or above %s", cwd)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	res, err := cfg.Resolve()
	if err != nil {
		return nil, err
	}
	if name != "" {
		res.Name = name
	}
	return &proto.Msg{
		Op: proto.OpUp,
		Spec: &proto.Spec{
			Name:     res.Name,
			Hostname: res.Name,
			Tools:    res.Tools,
		},
		EnvPolicy: &proto.EnvPolicy{Mode: res.ForwardEnv.Mode,
			Names: res.ForwardEnv.Names},
		Mounts:    res.Mounts,
		Machines:  res.Machines,
		Detail:    res.Lease, // the lease, carried in the one free text field
		ToolHosts: res.ToolHosts,
		CanMount:  res.CanMount,
		Config:    path,
		// The backend a new session opens on. Not a guess about anything: the
		// machine is chosen, and this is the choice a session starts with.
		Backend: res.Default,
	}, nil
}

func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	push := fs.Bool("push", false,
		"allow vibepod to put one binary in ~/.vp/bin on the machines this pod uses")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := loadSpec(fs.Arg(0))
	if err != nil {
		return err
	}
	if *push {
		m.Nodes = configHosts()
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := call(c, m); err != nil {
		return err
	}
	fmt.Printf("pod %s is up, on backend %s\n", m.Spec.Name, m.Backend)
	return nil
}

// cmdRun starts a process in a pod on this terminal's own descriptors.
func cmdRun(args []string) int {
	// Split on "--" before parsing flags: everything after it belongs to the
	// command being run, including any flags of its own.
	head, rest := args, []string(nil)
	if i := indexOf(args, "--"); i >= 0 {
		head, rest = args[:i], args[i+1:]
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	on := fs.String("on", "", "run it on this machine instead of the pod's default")
	if err := fs.Parse(head); err != nil {
		return 2
	}
	// With a "--", anything before it names the pod: `vp run work -- make`.
	// Without one, the whole tail is the command: `vp run claude`.
	name := ""
	if len(rest) > 0 {
		name = fs.Arg(0)
	} else {
		rest = fs.Args()
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "vp run: nothing to run; use -- cmd args")
		return 2
	}

	m, err := loadSpec(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	c, err := connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	defer c.Close()
	if err := ensureUp(c, m); err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	sess := &proto.Msg{
		Op:      proto.OpSession,
		Pod:     m.Spec.Name,
		Argv:    rest,
		Env:     os.Environ(),
		Cwd:     podCwd(m.Mounts),
		Backend: *on,
	}
	// On a terminal, run it on a pty the daemon owns, so it can be detached from
	// and come back to. Piped, run it directly on the caller's own descriptors:
	// there is nothing to detach from, and a pty would merge stdout with stderr,
	// which agents read separately.
	if term.IsTTY(os.Stdin) {
		code, err := attachOwningStdin(c, sess)
		if err != nil {
			fmt.Fprintln(os.Stderr, "vibepod:", err)
			return 1
		}
		return code
	}
	reply, err := call(c, sess, 0, 1, 2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	return reply.Code
}

// configHosts is every machine this project's config mentions, which is the scope
// `--push` consents to. It is deliberately the config's list and not "any machine
// vibepod ends up talking to".
func configHosts() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	path, found := config.Find(cwd)
	if !found {
		return nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil
	}
	return cfg.HostList()
}

// podCwd keeps the caller's directory when the pod can see it, which is the
// common case in a project, and falls back to the first mount.
func podCwd(mounts []proto.MountSpec) string {
	cwd, err := os.Getwd()
	if err == nil {
		for _, m := range mounts {
			if cwd == m.At || strings.HasPrefix(cwd, strings.TrimSuffix(m.At, "/")+"/") {
				return cwd
			}
		}
	}
	for _, m := range mounts {
		if !m.Identity {
			return m.At
		}
	}
	// No config to consult: the pod is already running and was opened from
	// somewhere else. Its own root is the one directory certain to exist.
	return "/"
}

func cmdPs() error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	reply, err := call(c, &proto.Msg{Op: proto.OpPs})
	if err != nil {
		return err
	}
	if len(reply.Pods) == 0 {
		fmt.Println("no pods running")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "POD\tPID\tMOUNTS\tDEFAULT\tUPTIME")
	for _, p := range reply.Pods {
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", p.Name, p.Pid, p.Mounts,
			p.Default, p.Uptime)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// A machine behind on a mount is refused for commands under that mount and
	// only that mount, so say exactly which.
	for _, p := range reply.Pods {
		for host, mounts := range p.Behind {
			fmt.Printf("%s: @%s is behind on %s\n", p.Name, host,
				strings.Join(mounts, ", "))
		}
	}
	// Which machine each session is on is the fact v2 added, so it gets its own
	// block rather than a column that would be empty for most pods.
	for _, p := range reply.Pods {
		if len(p.Live) == 0 {
			continue
		}
		fmt.Printf("\n%s sessions\n", p.Name)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  SESSION\tBACKEND\tKIND\tUPTIME\tRUNNING")
		for _, s := range p.Live {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", s.ID, s.Backend, s.Kind,
				s.Uptime, trim(strings.Join(s.Argv, " "), 30))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func cmdDown(args []string) error {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" {
		if m, err := loadSpec(""); err == nil {
			name = m.Spec.Name
		}
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := call(c, &proto.Msg{Op: proto.OpDown, Pod: name}); err != nil {
		return err
	}
	fmt.Printf("pod %s is down\n", name)
	return nil
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}
