package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"vibepod/internal/proto"
	"vibepod/internal/term"
)

// Sending one command to another machine.
//
// `vp @gpu03 rocm-smi` is the honest channel that replaced the exec gate. It is
// three words instead of zero, and in exchange it is a thing you can read in a
// log, reason about, and be sure of. The gate asked for none of those words and
// could not promise any of that.

// cmdDispatch runs one command on one machine, on this terminal's own
// descriptors: the daemon decides where and then stays out of the data path.
func cmdDispatch(backend string, argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintf(os.Stderr, "vp @%s: what should run there?\n", backend)
		return 2
	}
	if backend == "local" {
		backend = "pod"
	}
	return sendDispatch(&proto.Msg{Op: proto.OpDispatch, Backend: backend, Argv: argv})
}

// cmdTool is what a wrapper in /vp/bin calls. It names the tool rather than the
// machine, because the wrapper cannot know which session invoked it — the daemon
// resolves that, and names the alternatives when it cannot.
func cmdTool(name string, args []string) int {
	return sendDispatch(&proto.Msg{Op: proto.OpDispatch, Tool: name,
		Argv: append([]string{name}, args...)})
}

func sendDispatch(m *proto.Msg) int {
	c, err := connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	defer c.Close()
	cwd, _ := os.Getwd()
	m.Pod = podArg("")
	m.Cwd = cwd
	m.Env = os.Environ()
	m.Session = os.Getenv("VIBEPOD_SESSION")
	m.TTY = term.IsTTY(os.Stdin)
	if err := c.Send(m, 0, 1, 2); err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	// A dispatched command runs on another machine, where ssh will not forward a
	// Ctrl-C without a PTY — and a PTY would merge stderr into stdout, which
	// agents read separately. So pass signals to the daemon instead; it kills the
	// remote process group over a second multiplexed channel.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			_ = c.Send(&proto.Msg{Op: proto.OpSignal, Sig: int(s.(syscall.Signal))})
		}
	}()
	reply, _, err := c.Recv()
	signal.Stop(sigs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibepod: no answer from the daemon:", err)
		return exitUnavailable
	}
	switch reply.Op {
	case proto.OpExit:
		return reply.Code
	case proto.OpErr:
		fmt.Fprintln(os.Stderr, "vibepod:", reply.Err)
		return exitUnavailable
	}
	fmt.Fprintf(os.Stderr, "vibepod: unexpected reply %q\n", reply.Op)
	return exitUnavailable
}

// exitUnavailable is EX_TEMPFAIL: vibepod could not place this command. Distinct
// from anything the command itself would return, so an agent can tell "the
// machine was unreachable" from "the build failed".
const exitUnavailable = 75

// cmdUse moves this session's backend. In v1 this was gated behind a terminal
// check, on the reasoning that an agent must not re-route itself. In v2 an agent
// choosing its own machine is the entire interface, so the check is gone — and
// what it was protecting against, you not knowing where a command ran, is handled
// instead by the backend being explicit, per-session and on screen.
func cmdUse(args []string) error {
	fs := flag.NewFlagSet("use", flag.ContinueOnError)
	// Moving another session is for the machine that owns them. It exists because
	// a session on another machine cannot move itself: nothing is installed there,
	// so `vp` is not on that machine's PATH — which is the premise working as
	// intended rather than a gap. The cockpit's `b` is the same act with one key.
	other := fs.String("s", "", "move this session instead of the one you are in")
	// The machine is the natural first word — `vp use gpu03 -s 2` is what anyone
	// would type — and Go's flag package stops at the first operand, so take the
	// machine out before parsing rather than making the order matter.
	target, rest := "", []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-s" || a == "--s" {
			rest = append(rest, a)
			if i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		if target == "" {
			target = a
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("usage: vp use <machine|pod> [-s session]")
	}
	session := *other
	if session == "" {
		session = os.Getenv("VIBEPOD_SESSION")
	}
	if session == "" {
		return fmt.Errorf("no session here: `vp use` moves the session it is run " +
			"from, so run it in a pod terminal (`vp shell`), or name one with -s")
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	target = strings.TrimPrefix(target, "@")
	if target == "local" {
		target = "pod"
	}
	if _, err := call(c, &proto.Msg{Op: proto.OpUse, Pod: podArg(""),
		Session: session, Backend: target}); err != nil {
		return err
	}
	if *other != "" {
		fmt.Printf("session %s now runs on %s\n", session, target)
		return nil
	}
	fmt.Printf("this session now runs on %s\n", target)
	return nil
}

// cmdBackend answers in a word, because it is the thing a prompt asks.
func cmdBackend() error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	reply, err := call(c, &proto.Msg{Op: proto.OpBackend, Pod: podArg(""),
		Session: os.Getenv("VIBEPOD_SESSION")})
	if err != nil {
		return err
	}
	fmt.Println(reply.Backend)
	return nil
}

// cmdMount connects a machine and mounts a directory from it into a pod that is
// already running — which is the whole point. Adding a machine used to mean
// `down`, edit the YAML, `up`, and losing every session and the agent's context
// with them.
func cmdMount(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: vp mount <host>:/path [at]")
	}
	host, path, ok := strings.Cut(args[0], ":")
	if !ok || host == "" || !strings.HasPrefix(path, "/") {
		return fmt.Errorf("mount takes host:/absolute/path, not %q", args[0])
	}
	spec := proto.MountSpec{Host: host, Path: filepath.Clean(path)}
	if len(args) > 1 {
		spec.At = args[1]
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := call(c, &proto.Msg{Op: proto.OpMount, Pod: podArg(""),
		Mounts: []proto.MountSpec{spec}}); err != nil {
		return err
	}
	at := spec.At
	if at == "" {
		at = spec.Path
	}
	fmt.Printf("%s:%s is mounted at %s — `vp use %s` to run there\n",
		host, spec.Path, at, host)
	return nil
}

func cmdUnmount(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: vp unmount <path|@machine>")
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	m := &proto.Msg{Op: proto.OpUnmount, Pod: podArg("")}
	if strings.HasPrefix(args[0], "@") {
		m.Target = strings.TrimPrefix(args[0], "@")
	} else {
		m.Path = args[0]
	}
	if _, err := call(c, m); err != nil {
		return err
	}
	fmt.Printf("unmounted %s\n", args[0])
	return nil
}

// cmdSave writes the live state back to vibepod.yaml.
//
// It prints the file and asks, rather than overwriting: the file it replaces may
// have comments explaining why a mount is where it is, and this cannot preserve
// them. The config is a snapshot you take, and taking one should not quietly
// discard the reasoning in the last one.
func cmdSave(args []string) error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	reply, err := call(c, &proto.Msg{Op: proto.OpSave, Pod: podArg("")})
	if err != nil {
		return err
	}
	out := reply.Path
	if len(args) > 0 {
		out = args[0]
	}
	if out == "" {
		fmt.Print(reply.Detail)
		return nil
	}
	if _, err := os.Stat(out); err == nil && len(args) == 0 {
		fmt.Print(reply.Detail)
		fmt.Printf("\n# ^ the pod as it is running now\n")
		fmt.Printf("overwrite %s? comments in it will be lost [y/N] ", short(out))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Printf("not written; `vp save <file>` writes it elsewhere\n")
			return nil
		}
	}
	if err := os.WriteFile(out, []byte(reply.Detail), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", short(out))
	return nil
}

// cmdBrief prints the instructions the agent in this pod was given. Being able
// to read them is the point: in v2 the brief is the mechanism, so "what does the
// agent think is going on" has to be answerable.
func cmdBrief(args []string) error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	reply, err := call(c, &proto.Msg{Op: proto.OpBrief, Pod: podArg(name)})
	if err != nil {
		return err
	}
	fmt.Print(reply.Detail)
	return nil
}

// cmdNode is the surface for pods on other machines.
//
// Building one copies the vibepod binary into ~/.vp/bin there and nothing else —
// no credentials, no agent, no install outside that directory. Typing the command
// is the consent: it is the cheapest form that is still explicit, and `vibepod up
// --push` is the same grant for every machine the config names.
func cmdNode(args []string) error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	pod := podArg("")
	if len(args) == 0 {
		reply, err := call(c, &proto.Msg{Op: proto.OpNodeList, Pod: pod})
		if err != nil {
			return err
		}
		if len(reply.NodeInfos) == 0 {
			fmt.Println("no machine in this pod is running a pod of its own")
			fmt.Println("  `vp node add <machine>` builds one: the composed tree, at " +
				"the same paths, on that machine")
			return nil
		}
		for _, n := range reply.NodeInfos {
			state := fmt.Sprintf("generation %d", n.Generation)
			if !n.Reachable {
				state += " · not answering"
			}
			if len(n.Behind) > 0 {
				// Per mount, not per machine: commands under the mounts it does
				// have are still correct, and only the paths it is missing are
				// refused.
				state += " · behind on " + strings.Join(n.Behind, ", ")
			}
			fmt.Printf("@%s  %s\n", n.Host, state)
			for _, h := range n.Mounts {
				how := "cached from " + h.Host
				if h.Native {
					// Its own disk: no FUSE, no cache, no round trip. Running work
					// where the data lives is full speed with nothing to configure.
					how = "its own"
				}
				if h.ReadOnly {
					how += " · read-only"
				}
				fmt.Printf("  %-40s %s\n", short(h.At), how)
			}
		}
		return nil
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: vp node add <machine>")
		}
		if _, err := call(c, &proto.Msg{Op: proto.OpNodeAdd, Pod: pod,
			Target: args[1]}); err != nil {
			return err
		}
		fmt.Printf("%s is running a pod with this pod's mounts — `vp use %s`\n",
			args[1], args[1])
		return nil
	case "drop":
		if len(args) < 2 {
			return fmt.Errorf("usage: vp node drop <machine>")
		}
		if _, err := call(c, &proto.Msg{Op: proto.OpNodeDrop, Pod: pod,
			Target: args[1]}); err != nil {
			return err
		}
		fmt.Printf("the pod on %s is stopped\n", args[1])
		return nil
	}
	return fmt.Errorf("usage: vp node [add|drop <machine>]")
}
