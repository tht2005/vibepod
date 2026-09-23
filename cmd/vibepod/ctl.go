package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"vibepod/internal/config"
	"vibepod/internal/proto"
	"vibepod/internal/term"
)

const usage = `vpctl - run your agent here, run its commands where the code lives

  vpctl new [name]             create a pod and open the console
  vpctl up [name]              create a pod, detached
  vpctl run [name] -- cmd...   run a command in a pod, creating it if needed
  vpctl shell [name]           another terminal on a running pod
  vpctl attach [name] [sess]   return to a session you detached from
  vpctl ps                     running pods
  vpctl log [-f] [pod]         every command and the machine it ran on
  vpctl tree [pod]             mounts and live execs, in one view
  vpctl down [name]            stop a pod and release its mounts
  vpctl use <host|auto>        send this session's commands to a machine
  vpctl doctor                 check this machine can host a pod

A pod takes its name and mounts from ./vibepod.yaml unless you name one.
`

func runCtl(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 2
	}
	var err error
	switch args[0] {
	case "new":
		return cmdNew(args[1:])
	case "up":
		err = cmdUp(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "shell":
		return cmdShell(args[1:])
	case "attach":
		return cmdAttach(args[1:])
	case "ps":
		err = cmdPs()
	case "log":
		err = cmdLog(args[1:])
	case "tree":
		err = cmdTree(args[1:])
	case "down":
		err = cmdDown(args[1:])
	case "use":
		err = cmdUse(args[1:])
	case "doctor":
		err = cmdDoctor()
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "vpctl: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	return 0
}

// ensureUp creates the pod if it is not already running.
func ensureUp(c *proto.Conn, m *proto.Msg) error {
	if _, err := call(c, m); err != nil &&
		!strings.Contains(err.Error(), "already running") {
		return err
	}
	return nil
}

// loadSpec turns the project config into an up request.
func loadSpec(name string, shimAll bool) (*proto.Msg, error) {
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
	remotes := make([]proto.RemoteMount, 0, len(res.Remotes))
	for _, rm := range res.Remotes {
		remotes = append(remotes, proto.RemoteMount{Host: rm.Host, Path: rm.Path,
			At: rm.At, ReadOnly: rm.ReadOnly, Mode: rm.Mode})
	}
	return &proto.Msg{
		Op: proto.OpUp,
		Spec: &proto.Spec{
			Name:     res.Name,
			Binds:    res.Binds,
			Hostname: res.Name,
		},
		Routes:      res.Routes,
		Remotes:     remotes,
		ExecDefault: res.ExecDefault,
		ShimAll:     shimAll,
	}, nil
}

func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	shimAll := fs.Bool("shim-all", false, "shim every binary, whatever the route (debugging)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name := fs.Arg(0)
	m, err := loadSpec(name, *shimAll)
	if err != nil {
		return err
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := call(c, m); err != nil {
		return err
	}
	fmt.Printf("pod %s is up\n", m.Spec.Name)
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
	shimAll := fs.Bool("shim-all", false, "shim every binary, whatever the route (debugging)")
	if err := fs.Parse(head); err != nil {
		return 2
	}
	name := fs.Arg(0)
	if len(rest) == 0 {
		rest = fs.Args()
		if len(rest) > 0 {
			name, rest = "", rest
		}
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "vpctl run: nothing to run; use -- cmd args")
		return 2
	}

	m, err := loadSpec(name, *shimAll)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	c, err := connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	defer c.Close()
	if err := ensureUp(c, m); err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	sess := &proto.Msg{
		Op:   proto.OpSession,
		Pod:  m.Spec.Name,
		Argv: rest,
		Env:  os.Environ(),
		Cwd:  podCwd(m.Spec.Binds),
	}
	// On a terminal, run it on a pty the daemon owns, so it can be detached
	// from and come back to. Piped, run it directly on the caller's own
	// descriptors: there is nothing to detach from, and a pty would merge
	// stdout with stderr, which agents read separately.
	if term.IsTTY(os.Stdin) {
		code, err := attachClient(c, sess)
		if err != nil {
			fmt.Fprintln(os.Stderr, "vpctl:", err)
			return 1
		}
		return code
	}
	reply, err := call(c, sess, 0, 1, 2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	return reply.Code
}

// podCwd keeps the caller's directory when the pod can see it, which is the
// common case in a project, and falls back to the first mount.
func podCwd(binds []proto.Bind) string {
	cwd, err := os.Getwd()
	if err == nil {
		for _, b := range binds {
			if cwd == b.Dst || strings.HasPrefix(cwd, strings.TrimSuffix(b.Dst, "/")+"/") {
				return cwd
			}
		}
	}
	if len(binds) > 0 {
		return binds[0].Dst
	}
	return "/"
}

// cmdUse pins the current session's executor. Prefer exec_on: in the config
// for anything durable: a pin is invisible in the config and outlives your
// memory of setting it.
func cmdUse(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: vpctl use <host|auto>")
	}
	session := os.Getenv("VIBEPOD_SESSION")
	if session == "" {
		return fmt.Errorf("no session here; run this inside a pod terminal")
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = call(c, &proto.Msg{Op: proto.OpUse, Pod: os.Getenv("VIBEPOD_POD"),
		Session: session, Target: args[0]})
	if err != nil {
		return err
	}
	fmt.Printf("commands from this session now run on %s\n", args[0])
	return nil
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
	fmt.Fprintln(w, "POD\tPID\tSESSIONS\tMOUNTS\tUPTIME")
	for _, p := range reply.Pods {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n", p.Name, p.Pid, p.Sessions, p.Mounts, p.Uptime)
	}
	return w.Flush()
}

func cmdDown(args []string) error {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" {
		if m, err := loadSpec("", false); err == nil {
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

func cmdDoctor() error {
	self, _ := os.Executable()
	checks := []struct {
		name string
		err  error
	}{
		{"vpsh binary", statErr(filepath.Join(filepath.Dir(self), "vpsh"))},
		{"unprivileged user namespaces", checkUserns()},
		{"seccomp user notification", checkSeccomp()},
	}
	bad := 0
	for _, c := range checks {
		if c.err != nil {
			bad++
			fmt.Printf("  ✗ %-32s %v\n", c.name, c.err)
		} else {
			fmt.Printf("  ✓ %-32s\n", c.name)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d check(s) failed", bad)
	}
	return nil
}

func statErr(p string) error {
	_, err := os.Stat(p)
	return err
}
