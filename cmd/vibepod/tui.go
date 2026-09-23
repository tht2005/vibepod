package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/sys"
	"vibepod/internal/term"
)

// The cockpit.
//
// It is a cockpit and not a multiplexer, and that is the whole design. Running
// Claude Code inside a homemade terminal emulator means nested alt-screens,
// mouse reporting fighting mouse reporting, bracketed paste arriving mangled,
// and resize storms — a large budget spent reimplementing tmux badly, with a
// worse `claude` at the end of it.
//
// So ⏎ does not render a terminal in a pane. It leaves the alt-screen, hands the
// raw terminal to the session, and redraws when you come back: what lazygit does
// with $EDITOR. That costs a tenth of the work, has none of those failure modes,
// and gives up only side-by-side panes, which tmux already provides for anyone
// who wants them.
//
// The detach key therefore belongs to the session, not to this: swallowing
// keystrokes that Claude Code wants is exactly the failure being avoided.

type cockpit struct {
	pod   string
	state *term.State

	mu       sync.Mutex
	hosts    []proto.HostInfo
	sessions []proto.SessionInfo
	mounts   []proto.TreeMount
	activity []string
	// deflt is the backend a new session opens on, which is what the status line
	// says when no session is focused.
	deflt   string
	filter  string
	status  string
	focus   int // 0: machines, 1: sessions
	selHost int
	selSess int
	rows    int
	cols    int
	// prompt is non-nil while the bottom line is asking for something.
	prompt *prompt
	// handoff is the session's connection while it owns the terminal. The one
	// read loop forwards to it instead of editing a line, which is why the
	// daemon never has to read this terminal itself.
	handoff *proto.Conn
	quit    bool
}

// prompt is a one-line question: mounting needs a host:path, moving a backend
// needs a machine.
type prompt struct {
	label  string
	line   []rune
	action func(*cockpit, string)
}

const (
	altOn  = "\x1b[?1049h"
	altOff = "\x1b[?1049l"
)

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
	co := &cockpit{pod: m.Spec.Name, rows: 24, cols: 80,
		status: "pod " + m.Spec.Name + " is up"}
	return co.run()
}

func (co *cockpit) run() int {
	state, err := term.MakeRaw(os.Stdin.Fd())
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	co.state = state
	defer func() {
		os.Stdout.WriteString(altOff)
		state.Restore()
	}()
	if r, c, err := sys.GetWinsize(os.Stdin.Fd()); err == nil {
		co.rows, co.cols = r, c
	}
	os.Stdout.WriteString(altOn)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			if r, c, err := sys.GetWinsize(os.Stdin.Fd()); err == nil {
				co.mu.Lock()
				co.rows, co.cols = r, c
				hand := co.handoff
				co.mu.Unlock()
				if hand != nil {
					_ = hand.Send(&proto.Msg{Op: proto.OpWinch, Rows: r, Cols: c})
					continue
				}
				co.draw()
			}
		}
	}()

	go co.followEvents()
	co.refresh()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			co.mu.Lock()
			busy := co.handoff != nil || co.quit
			co.mu.Unlock()
			if busy {
				continue
			}
			co.refresh()
		}
	}()

	// One read loop for the cockpit's whole life. While a session owns the
	// terminal the same loop forwards keystrokes to it, which is what "hands the
	// terminal over" means in practice.
	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return 0
		}
		co.mu.Lock()
		hand := co.handoff
		co.mu.Unlock()
		if hand != nil {
			if !sendInput(hand, buf[:n]) {
				// Ctrl-\ : the session keeps running and we take the screen back.
				continue
			}
			continue
		}
		if co.keys(buf[:n]) {
			return 0
		}
	}
}

// keys handles one read's worth of input. Returns true to quit.
func (co *cockpit) keys(b []byte) bool {
	for i := 0; i < len(b); i++ {
		ch := b[i]
		// Arrow keys, which are the ones people reach for first.
		if ch == 0x1b && i+2 < len(b) && b[i+1] == '[' {
			switch b[i+2] {
			case 'A':
				co.move(-1)
			case 'B':
				co.move(1)
			case 'C':
				co.setFocus(1)
			case 'D':
				co.setFocus(0)
			}
			i += 2
			continue
		}
		if co.typing(ch) {
			continue
		}
		switch ch {
		case 'q', 0x03, 0x04:
			return true
		case 'j':
			co.move(1)
		case 'k':
			co.move(-1)
		case '\t':
			co.mu.Lock()
			co.focus = 1 - co.focus
			co.mu.Unlock()
			co.draw()
		case '\r', '\n':
			co.attachSelected()
		case 'm':
			co.ask("mount (host:/path): ", func(co *cockpit, line string) {
				co.run1("mount", strings.Fields(line)...)
			})
		case 'u':
			co.ask("unmount (path or @machine): ", func(co *cockpit, line string) {
				co.run1("unmount", line)
			})
		case 'b':
			co.ask("backend for this session: ", func(co *cockpit, line string) {
				co.setBackend(line)
			})
		case '/':
			co.ask("filter: ", func(co *cockpit, line string) {
				co.mu.Lock()
				co.filter = line
				co.mu.Unlock()
			})
		case 'r':
			co.refresh()
		}
	}
	return false
}

// typing feeds the bottom-line prompt while one is open. Returns true if the
// keystroke belonged to it.
func (co *cockpit) typing(ch byte) bool {
	co.mu.Lock()
	p := co.prompt
	co.mu.Unlock()
	if p == nil {
		return false
	}
	switch {
	case ch == '\r' || ch == '\n':
		line := strings.TrimSpace(string(p.line))
		co.mu.Lock()
		co.prompt = nil
		co.mu.Unlock()
		if line != "" || p.label == "filter: " {
			p.action(co, line)
		}
		co.refresh()
	case ch == 0x1b || ch == 0x03:
		co.mu.Lock()
		co.prompt = nil
		co.mu.Unlock()
		co.draw()
	case ch == 0x7f || ch == 0x08:
		co.mu.Lock()
		if len(p.line) > 0 {
			p.line = p.line[:len(p.line)-1]
		}
		co.mu.Unlock()
		co.draw()
	case ch >= 0x20:
		co.mu.Lock()
		p.line = append(p.line, rune(ch))
		co.mu.Unlock()
		co.draw()
	}
	return true
}

func (co *cockpit) ask(label string, action func(*cockpit, string)) {
	co.mu.Lock()
	co.prompt = &prompt{label: label, action: action}
	co.mu.Unlock()
	co.draw()
}

func (co *cockpit) move(d int) {
	co.mu.Lock()
	if co.focus == 0 {
		co.selHost = clamp(co.selHost+d, len(co.hosts))
	} else {
		co.selSess = clamp(co.selSess+d, len(co.sessions))
	}
	co.mu.Unlock()
	co.draw()
}

func (co *cockpit) setFocus(f int) {
	co.mu.Lock()
	co.focus = f
	co.mu.Unlock()
	co.draw()
}

func clamp(i, n int) int {
	if n == 0 {
		return 0
	}
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

// run1 runs one control command and shows what it said on the status line.
func (co *cockpit) run1(verb string, args ...string) {
	c, err := connect()
	if err != nil {
		co.say("vibepod: " + err.Error())
		return
	}
	defer c.Close()
	var m *proto.Msg
	switch verb {
	case "mount":
		if len(args) == 0 {
			return
		}
		host, path, ok := strings.Cut(args[0], ":")
		if !ok || !strings.HasPrefix(path, "/") {
			co.say("mount takes host:/absolute/path")
			return
		}
		spec := proto.MountSpec{Host: host, Path: path}
		if len(args) > 1 {
			spec.At = args[1]
		}
		m = &proto.Msg{Op: proto.OpMount, Pod: co.pod,
			Mounts: []proto.MountSpec{spec}}
	case "unmount":
		m = &proto.Msg{Op: proto.OpUnmount, Pod: co.pod}
		if strings.HasPrefix(args[0], "@") {
			m.Target = strings.TrimPrefix(args[0], "@")
		} else {
			m.Path = args[0]
		}
	default:
		return
	}
	co.say(verb + "…")
	if _, err := call(c, m); err != nil {
		co.say(verb + ": " + err.Error())
		return
	}
	co.say(verb + " " + strings.Join(args, " ") + " — done")
}

// setBackend moves the focused session, which is the key the whole model turns
// on: the machine is chosen, and this is where a human chooses it.
func (co *cockpit) setBackend(target string) {
	co.mu.Lock()
	var id string
	if co.selSess < len(co.sessions) {
		id = co.sessions[co.selSess].ID
	}
	co.mu.Unlock()
	if id == "" {
		co.say("no session selected; ⇥ to the session list first")
		return
	}
	c, err := connect()
	if err != nil {
		co.say("vibepod: " + err.Error())
		return
	}
	defer c.Close()
	if _, err := call(c, &proto.Msg{Op: proto.OpUse, Pod: co.pod, Session: id,
		Backend: strings.TrimPrefix(target, "@")}); err != nil {
		co.say(err.Error())
		return
	}
	co.say("session " + id + " is now on " + target)
}

// attachSelected hands the terminal to the focused session.
//
// It does not wait for the session to finish, and that is not an optimisation:
// the loop that called this is the same loop that has to forward keystrokes to
// the session, so waiting here would hand over a terminal nobody is reading. The
// console this replaced learned that the hard way.
func (co *cockpit) attachSelected() {
	co.mu.Lock()
	var id string
	if co.selSess < len(co.sessions) {
		id = co.sessions[co.selSess].ID
	}
	co.mu.Unlock()
	if id == "" {
		co.say("nothing to attach to — `vp shell` opens a new terminal")
		return
	}
	c, err := connect()
	if err != nil {
		co.say("vibepod: " + err.Error())
		return
	}
	// Claim the keyboard before anything is written, so that a keystroke arriving
	// while the session is opening reaches the session rather than being read as
	// a cockpit key.
	co.mu.Lock()
	co.handoff = c
	co.mu.Unlock()

	// Leave the alt-screen: from here the session owns the real screen, and it
	// expects to find it the way any program expects to find a terminal.
	os.Stdout.WriteString(altOff)
	state, err := attachSend(c, &proto.Msg{Op: proto.OpAttach, Pod: co.pod,
		Session: id})
	if err != nil {
		co.mu.Lock()
		co.handoff = nil
		co.mu.Unlock()
		c.Close()
		os.Stdout.WriteString(altOn)
		co.say("attach: " + err.Error())
		return
	}
	go func() {
		code, detached, werr := attachWait(c)
		co.mu.Lock()
		co.handoff = nil
		co.mu.Unlock()
		state.Restore()
		c.Close()
		// Back to raw, back to the alt-screen, and redraw: the cockpit was not
		// running while the session had the screen, so there is nothing to catch
		// up on — only a frame to paint.
		if st, err := term.MakeRaw(os.Stdin.Fd()); err == nil {
			co.state = st
		}
		os.Stdout.WriteString(altOn)
		switch {
		case werr != nil:
			co.say("attach: " + werr.Error())
		case detached:
			co.say("session " + id + " left running")
		default:
			co.say(fmt.Sprintf("session %s ended (%d)", id, code))
		}
		co.refresh()
	}()
}

func (co *cockpit) say(s string) {
	co.mu.Lock()
	co.status = s
	co.mu.Unlock()
	co.draw()
}

// refresh asks the daemon for the state the panes show.
func (co *cockpit) refresh() {
	c, err := connect()
	if err != nil {
		co.say("vibepod: " + err.Error())
		return
	}
	defer c.Close()
	hosts, _ := call(c, &proto.Msg{Op: proto.OpHosts, Pod: co.pod})
	tree, _ := call(c, &proto.Msg{Op: proto.OpTree, Pod: co.pod})
	co.mu.Lock()
	if hosts != nil {
		co.hosts = hosts.Hosts
	}
	if tree != nil && tree.Tree != nil {
		co.mounts = tree.Tree.Mounts
		co.deflt = tree.Tree.Default
		co.sessions = co.sessions[:0]
		for _, s := range tree.Tree.Sessions {
			co.sessions = append(co.sessions, proto.SessionInfo{ID: s.ID,
				Kind: s.Kind, Backend: s.Backend})
		}
	}
	co.selHost = clamp(co.selHost, len(co.hosts))
	co.selSess = clamp(co.selSess, len(co.sessions))
	co.mu.Unlock()
	co.draw()
}

// followEvents is the activity pane: the daemon's own event stream, which is the
// same stream `vp log -f` reads. One source, three renderings.
func (co *cockpit) followEvents() {
	for {
		c, err := connect()
		if err == nil {
			if err := c.Send(&proto.Msg{Op: proto.OpLog, Pod: co.pod,
				Follow: true}); err == nil {
				for {
					m, _, err := c.Recv()
					if err != nil {
						break
					}
					if m.Op != proto.OpEvent {
						continue
					}
					var e event.Event
					if json.Unmarshal(m.Event, &e) != nil {
						continue
					}
					if line := logLine(e, true, false); line != "" {
						co.push(line)
					}
				}
			}
			c.Close()
		}
		time.Sleep(time.Second)
	}
}

func (co *cockpit) push(line string) {
	co.mu.Lock()
	co.activity = append(co.activity, line)
	if len(co.activity) > 500 {
		co.activity = co.activity[len(co.activity)-500:]
	}
	hand := co.handoff
	co.mu.Unlock()
	if hand == nil {
		co.draw()
	}
}
