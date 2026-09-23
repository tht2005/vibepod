package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"vibepod/internal/config"
	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/term"
	"vibepod/internal/ui"
)

// The cockpit's view lives in internal/ui, next to `vp shell`; this is what it
// does to the world, over the daemon connection only the command line holds.

func cmdCockpit(args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
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
	if err := ensureUp(c, m); err != nil {
		c.Close()
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	c.Close()
	if !term.IsTTY(os.Stdin) {
		fmt.Fprintln(os.Stderr, "vibepod: the cockpit needs a terminal; "+
			"`vibepod up` is the same thing without one")
		return 1
	}
	pod := m.Spec.Name
	err = ui.RunCockpit(ui.CockpitConfig{
		Pod:  pod,
		Load: func() ui.CockpitState { return loadCockpit(pod) },
		Mount: func(line string) error {
			args := strings.Fields(line)
			if len(args) == 0 {
				return nil
			}
			host, path, at, err := config.ParseRemote(args[0])
			if err != nil {
				return err
			}
			spec := proto.MountSpec{Host: host, Path: path, At: at}
			if len(args) > 1 {
				spec.At = args[1]
			}
			return oneCall(&proto.Msg{Op: proto.OpMount, Pod: pod,
				Mounts: []proto.MountSpec{spec}})
		},
		Unmount: func(arg string) error {
			m := &proto.Msg{Op: proto.OpUnmount, Pod: pod}
			if strings.HasPrefix(arg, "@") {
				m.Target = strings.TrimPrefix(arg, "@")
			} else {
				m.Path = arg
			}
			return oneCall(m)
		},
		Use: func(session, machine string) error {
			return oneCall(&proto.Msg{Op: proto.OpUse, Pod: pod, Session: session,
				Backend: machine})
		},
		Attach: func(session string) *exec.Cmd {
			self, err := os.Executable()
			if err != nil {
				self = "vp"
			}
			cmd := exec.Command(self, "attach", pod, session)
			cmd.Args[0] = "vp"
			return cmd
		},
		Follow: func(send func(ui.Activity)) error { return followEvents(pod, send) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	return 0
}

func oneCall(m *proto.Msg) error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = call(c, m)
	return err
}

// loadCockpit asks the daemon for the state the panes show.
func loadCockpit(pod string) ui.CockpitState {
	c, err := connect()
	if err != nil {
		return ui.CockpitState{Err: err}
	}
	defer c.Close()
	var st ui.CockpitState
	hosts, _ := call(c, &proto.Msg{Op: proto.OpHosts, Pod: pod})
	tree, _ := call(c, &proto.Msg{Op: proto.OpTree, Pod: pod})
	ps, _ := call(c, &proto.Msg{Op: proto.OpPs})
	if ps != nil {
		for _, p := range ps.Pods {
			if p.Name == pod {
				st.Behind = p.Behind
			}
		}
	}
	if hosts != nil {
		st.Hosts = hosts.Hosts
	}
	if tree != nil && tree.Tree != nil {
		st.Mounts = tree.Tree.Mounts
		st.Default = tree.Tree.Default
		st.Sessions = []proto.SessionInfo{}
		for _, s := range tree.Tree.Sessions {
			st.Sessions = append(st.Sessions, proto.SessionInfo{ID: s.ID,
				Kind: s.Kind, Backend: s.Backend})
		}
	}
	return st
}

// followEvents is the activity pane: the daemon's own event stream, which is the
// same stream `vp log -f` reads. One source, three renderings.
func followEvents(pod string, send func(ui.Activity)) error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Send(&proto.Msg{Op: proto.OpLog, Pod: pod, Follow: true}); err != nil {
		return err
	}
	for {
		m, _, err := c.Recv()
		if err != nil {
			return err
		}
		if m.Op != proto.OpEvent {
			continue
		}
		var e event.Event
		if json.Unmarshal(m.Event, &e) != nil {
			continue
		}
		if a, ok := activity(e); ok {
			send(a)
		}
	}
}

// activity is one event as the log pane shows it.
func activity(e event.Event) (ui.Activity, bool) {
	a := ui.Activity{Machine: e.Target, PID: e.PID}
	if t, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
		a.Time = t
	}
	argv := condense(strings.Join(e.Argv, " "))
	switch e.Kind {
	case event.KindExec:
		a.What, a.Running = argv, true
	case event.KindExit:
		// vibepod is the parent of routed commands but only an observer of
		// pod-local ones, so a status is often genuinely unknown, and Code
		// stays nil rather than inventing a tick.
		a.What, a.Code, a.Dur = argv, e.Code, dur(e.ElapsedMS)
	case event.KindPod:
		a.Machine, a.What = "", "pod "+e.Pod+" "+e.Detail
	case event.KindSession:
		a.What = "session " + e.Session + " " + e.Detail
	case event.KindNotice:
		a.What = strings.TrimSpace(argv + "  " + e.Detail)
	default:
		return a, false
	}
	return a, true
}
