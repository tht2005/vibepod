package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"vibepod/internal/config"
	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/term"
	"vibepod/internal/ui"
)

// The cockpit.
//
// It is a cockpit and not a multiplexer, and that is the whole design. Running
// Claude Code inside a homemade terminal emulator means nested alt-screens,
// mouse reporting fighting mouse reporting, bracketed paste arriving mangled,
// and resize storms — a large budget spent reimplementing tmux badly, with a
// worse `claude` at the end of it.
//
// So ⏎ does not render a terminal in a pane. It leaves the alt-screen and runs
// `vp attach` on the real terminal, and redraws when that returns: what lazygit
// does with $EDITOR. Which interface the session comes back in is `vp attach`'s
// business — blocks for a session `vp shell` started, the raw pty for anything
// else — so there is one way to look at a session, whichever door you use.
//
// The detach key therefore belongs to the session, not to this: swallowing
// keystrokes that Claude Code wants is exactly the failure being avoided.

type cockpit struct {
	pod string

	hosts    []proto.HostInfo
	sessions []proto.SessionInfo
	mounts   []proto.TreeMount
	activity []string
	// deflt is the backend a new session opens on, which is what the status line
	// says when no session is focused.
	deflt string
	// behind is, per machine, the mounts its node pod does not hold yet. Shown on
	// that machine's row, because a command there under one of them is refused.
	behind  map[string][]string
	filter  string
	status  string
	focus   int // 0: machines, 1: sessions
	selHost int
	selSess int
	w, h    int
	// asking is the bottom line's question while one is open: mounting needs a
	// host:path, moving a backend needs a machine.
	asking *question
	input  textinput.Model
}

type question struct {
	label  string
	action func(line string) tea.Cmd
	// empty is whether an empty answer is still an answer: clearing a filter is.
	empty bool
}

type (
	cockpitState struct {
		hosts    []proto.HostInfo
		sessions []proto.SessionInfo
		mounts   []proto.TreeMount
		deflt    string
		behind   map[string][]string
		err      error
	}
	activityMsg string
	statusMsg   string
	refreshTick struct{}
	attachedMsg struct {
		id  string
		err error
	}
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
	in := textinput.New()
	in.Prompt = ""
	co := &cockpit{pod: m.Spec.Name, status: "pod " + m.Spec.Name + " is up",
		input: in}
	p := tea.NewProgram(co)
	go followEvents(p, co.pod)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	return 0
}

func (co *cockpit) Init() tea.Cmd { return tea.Batch(co.refresh(), tick()) }

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return refreshTick{} })
}

func (co *cockpit) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		co.w, co.h = msg.Width, msg.Height
	case cockpitState:
		if msg.err != nil {
			co.status = "vibepod: " + msg.err.Error()
			break
		}
		co.hosts, co.mounts, co.deflt, co.behind = msg.hosts, msg.mounts, msg.deflt, msg.behind
		if msg.sessions != nil {
			co.sessions = msg.sessions
		}
		co.selHost = clamp(co.selHost, len(co.hosts))
		co.selSess = clamp(co.selSess, len(co.sessions))
	case refreshTick:
		return co, tea.Batch(co.refresh(), tick())
	case activityMsg:
		co.activity = append(co.activity, string(msg))
		if len(co.activity) > 500 {
			co.activity = co.activity[len(co.activity)-500:]
		}
	case statusMsg:
		co.status = string(msg)
		return co, co.refresh()
	case attachedMsg:
		return co, co.afterAttach(msg)
	case tea.KeyPressMsg:
		return co, co.key(msg)
	}
	return co, nil
}

func (co *cockpit) key(k tea.KeyPressMsg) tea.Cmd {
	if q := co.asking; q != nil {
		switch k.String() {
		case "enter":
			line := strings.TrimSpace(co.input.Value())
			co.asking = nil
			if line != "" || q.empty {
				return q.action(line)
			}
			return nil
		case "esc", "ctrl+c":
			co.asking = nil
			return nil
		}
		var cmd tea.Cmd
		co.input, cmd = co.input.Update(k)
		return cmd
	}
	switch k.String() {
	case "q", "ctrl+c", "ctrl+d":
		return tea.Quit
	case "down", "j":
		co.move(1)
	case "up", "k":
		co.move(-1)
	case "right":
		co.focus = 1
	case "left":
		co.focus = 0
	case "tab":
		co.focus = 1 - co.focus
	case "enter":
		return co.attachSelected()
	case "m":
		return co.ask("mount (host:/path): ", false, func(line string) tea.Cmd {
			return co.control("mount", strings.Fields(line)...)
		})
	case "u":
		return co.ask("unmount (path or @machine): ", false, func(line string) tea.Cmd {
			return co.control("unmount", line)
		})
	case "b":
		return co.ask("backend for this session: ", false, co.setBackend)
	case "/":
		return co.ask("filter: ", true, func(line string) tea.Cmd {
			co.filter = line
			return nil
		})
	case "r":
		return co.refresh()
	}
	return nil
}

func (co *cockpit) ask(label string, empty bool, action func(string) tea.Cmd) tea.Cmd {
	co.asking = &question{label: label, action: action, empty: empty}
	co.input.Reset()
	if label == "filter: " {
		co.input.SetValue(co.filter)
		co.input.CursorEnd()
	}
	return co.input.Focus()
}

func (co *cockpit) move(d int) {
	if co.focus == 0 {
		co.selHost = clamp(co.selHost+d, len(co.hosts))
	} else {
		co.selSess = clamp(co.selSess+d, len(co.sessions))
	}
}

func clamp(i, n int) int {
	if n == 0 || i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

func (co *cockpit) selected() string {
	if co.selSess < len(co.sessions) {
		return co.sessions[co.selSess].ID
	}
	return ""
}

// control runs one control command and puts what it said on the status line.
func (co *cockpit) control(verb string, args ...string) tea.Cmd {
	if len(args) == 0 {
		return nil
	}
	var m *proto.Msg
	switch verb {
	case "mount":
		host, path, at, err := config.ParseRemote(args[0])
		if err != nil {
			co.status = err.Error()
			return nil
		}
		spec := proto.MountSpec{Host: host, Path: path, At: at}
		if len(args) > 1 {
			spec.At = args[1]
		}
		m = &proto.Msg{Op: proto.OpMount, Pod: co.pod, Mounts: []proto.MountSpec{spec}}
	case "unmount":
		m = &proto.Msg{Op: proto.OpUnmount, Pod: co.pod}
		if strings.HasPrefix(args[0], "@") {
			m.Target = strings.TrimPrefix(args[0], "@")
		} else {
			m.Path = args[0]
		}
	}
	co.status = verb + "…"
	return func() tea.Msg {
		if err := oneCall(m); err != nil {
			return statusMsg(verb + ": " + err.Error())
		}
		return statusMsg(verb + " " + strings.Join(args, " ") + " — done")
	}
}

// setBackend moves the focused session, which is the key the whole model turns
// on: the machine is chosen, and this is where a human chooses it.
func (co *cockpit) setBackend(target string) tea.Cmd {
	id := co.selected()
	if id == "" {
		co.status = "no session selected; ⇥ to the session list first"
		return nil
	}
	target = strings.TrimPrefix(target, "@")
	return func() tea.Msg {
		if err := oneCall(&proto.Msg{Op: proto.OpUse, Pod: co.pod, Session: id,
			Backend: target}); err != nil {
			return statusMsg(err.Error())
		}
		return statusMsg("session " + id + " is now on " + target)
	}
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

// attachSelected gives the terminal to `vp attach` for the focused session.
func (co *cockpit) attachSelected() tea.Cmd {
	id := co.selected()
	if id == "" {
		co.status = "nothing to attach to — `vp shell` opens a new terminal"
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		co.status = "attach: " + err.Error()
		return nil
	}
	cmd := exec.Command(self, "attach", co.pod, id)
	cmd.Args[0] = "vp"
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return attachedMsg{id, err} })
}

// afterAttach says how the session was left, which only the daemon still
// knows: `vp attach` has exited either way.
func (co *cockpit) afterAttach(msg attachedMsg) tea.Cmd {
	return func() tea.Msg {
		st := loadCockpit(co.pod)
		for _, s := range st.sessions {
			if s.ID == msg.id {
				return statusMsg("session " + msg.id + " left running")
			}
		}
		if ee, ok := msg.err.(*exec.ExitError); ok {
			return statusMsg(fmt.Sprintf("session %s ended (%d)", msg.id, ee.ExitCode()))
		}
		return statusMsg("session " + msg.id + " ended")
	}
}

func (co *cockpit) refresh() tea.Cmd {
	pod := co.pod
	return func() tea.Msg { return loadCockpit(pod) }
}

// loadCockpit asks the daemon for the state the panes show.
func loadCockpit(pod string) cockpitState {
	c, err := connect()
	if err != nil {
		return cockpitState{err: err}
	}
	defer c.Close()
	var st cockpitState
	hosts, _ := call(c, &proto.Msg{Op: proto.OpHosts, Pod: pod})
	tree, _ := call(c, &proto.Msg{Op: proto.OpTree, Pod: pod})
	ps, _ := call(c, &proto.Msg{Op: proto.OpPs})
	if ps != nil {
		for _, p := range ps.Pods {
			if p.Name == pod {
				st.behind = p.Behind
			}
		}
	}
	if hosts != nil {
		st.hosts = hosts.Hosts
	}
	if tree != nil && tree.Tree != nil {
		st.mounts = tree.Tree.Mounts
		st.deflt = tree.Tree.Default
		st.sessions = []proto.SessionInfo{}
		for _, s := range tree.Tree.Sessions {
			st.sessions = append(st.sessions, proto.SessionInfo{ID: s.ID,
				Kind: s.Kind, Backend: s.Backend})
		}
	}
	return st
}

// followEvents is the activity pane: the daemon's own event stream, which is the
// same stream `vp log -f` reads. One source, three renderings.
func followEvents(p *tea.Program, pod string) {
	for {
		c, err := connect()
		if err == nil {
			if err := c.Send(&proto.Msg{Op: proto.OpLog, Pod: pod, Follow: true}); err == nil {
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
						p.Send(activityMsg(line))
					}
				}
			}
			c.Close()
		}
		time.Sleep(time.Second)
	}
}

// Drawing.

var (
	coTitle   = lipgloss.NewStyle().Bold(true)
	coHead    = lipgloss.NewStyle().Bold(true)
	coDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	coSel     = lipgloss.NewStyle().Reverse(true)
	coKeys    = lipgloss.NewStyle().Reverse(true)
	coDivider = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

func (co *cockpit) View() tea.View {
	v := tea.NewView(co.frame())
	v.AltScreen = true
	return v
}

func (co *cockpit) frame() string {
	w, h := co.w, co.h
	if w == 0 {
		return ""
	}
	if h < 8 || w < 40 {
		// Too small to divide. Say so rather than drawing something broken.
		return "vibepod: the cockpit needs at least 40x8"
	}
	left := min(w/2, 40)
	right := w - left - 1
	body := h - 3 // title, status, keys

	var b strings.Builder
	b.WriteString(fit(coTitle.Render("vibepod · "+co.pod), w) + "\n")
	l, r := co.machineLines(left, body), co.activityLines(right, body)
	for i := 0; i < body; i++ {
		a, c := "", ""
		if i < len(l) {
			a = l[i]
		}
		if i < len(r) {
			c = r[i]
		}
		b.WriteString(fit(a, left) + coDivider.Render("│") + fit(c, right) + "\n")
	}
	// Status, then the keymap. The backend of the focused session is on the
	// keymap line because it is the thing you most need to know before pressing
	// a key.
	status := coDim.Render(co.status)
	if co.asking != nil {
		status = co.asking.label + co.input.View()
	}
	b.WriteString(fit(status, w) + "\n")
	b.WriteString(coKeys.Render(fit(co.keyline(), w)))
	return b.String()
}

// fit makes s exactly w columns wide, cutting or padding.
func fit(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	if n := lipgloss.Width(s); n < w {
		s += strings.Repeat(" ", w-n)
	}
	return s
}

func (co *cockpit) keyline() string {
	backend := co.deflt
	if co.selSess < len(co.sessions) {
		backend = co.sessions[co.selSess].Backend
	}
	if backend == "" {
		backend = "—"
	}
	return fmt.Sprintf(" backend %s │ m mount  u unmount  b backend  ⏎ attach  "+
		"⇥ pane  / filter  q quit", backend)
}

// machineLines is the left column: machines, sessions, mounts. All three are
// lists of things you act on, so they share a column and the focus moves between
// them.
//
// The budget matters more than it sounds. A well-used ssh config has thirty
// hosts, and listing every machine this pod *could* mount would push the
// sessions — the thing you came to look at — off the bottom of the screen. So
// mounted machines are always shown, and the unmounted ones take whatever room
// is left over after the sessions and the mounts have had theirs.
func (co *cockpit) machineLines(w, avail int) []string {
	var mounted, unmounted []proto.HostInfo
	for _, h := range co.hosts {
		if h.Mounted {
			mounted = append(mounted, h)
		} else {
			unmounted = append(unmounted, h)
		}
	}

	sessions := []string{"", coHead.Render("SESSIONS")}
	if len(co.sessions) == 0 {
		sessions = append(sessions, coDim.Render("  none — `vp shell` opens one"))
	}
	for i, s := range co.sessions {
		kind := s.Kind
		if kind == "" {
			kind = "session"
		}
		on := lipgloss.NewStyle().Foreground(ui.MachineColor(s.Backend)).Render(s.Backend)
		row := fmt.Sprintf("%-3s %-10s ▸ %s", s.ID, kind, on)
		sessions = append(sessions, mark(row, w, co.focus == 1 && i == co.selSess))
	}
	mounts := []string{"", coHead.Render("MOUNTS")}
	for _, m := range co.mounts {
		mounts = append(mounts, fmt.Sprintf("  %-16s %s", short(m.At), shortSource(m)))
	}

	out := []string{coHead.Render("MACHINES")}
	room := avail - 1 - len(sessions) - len(mounts) - len(mounted)
	shown := min(len(unmounted), max(room, 0))
	for i, h := range co.hosts {
		if h.Mounted {
			out = append(out, mark(co.hostRow(h), w, co.focus == 0 && i == co.selHost))
		}
	}
	n := shown
	for i, h := range co.hosts {
		if h.Mounted {
			continue
		}
		if n == 0 {
			break
		}
		n--
		out = append(out, mark(co.hostRow(h), w, co.focus == 0 && i == co.selHost))
	}
	if more := len(unmounted) - shown; more > 0 {
		out = append(out, coDim.Render(fmt.Sprintf("  …%d more this pod could mount", more)))
	}
	out = append(out, sessions...)
	// A section header with nothing under it is worse than no section, and on a
	// short terminal the mounts are the part you can go and read elsewhere.
	if avail-len(out) >= len(mounts) || avail-len(out) > 2 {
		out = append(out, mounts...)
	}
	return out
}

func (co *cockpit) hostRow(h proto.HostInfo) string {
	dot := "●"
	switch {
	case h.Local:
	case !h.Mounted:
		dot = "○"
	case !h.Connected:
		dot = "◌"
	}
	where := "local"
	switch {
	case !h.Mounted:
		where = coDim.Render("vp mount " + h.Name + ":/path")
	case len(h.Dirs) > 0:
		where = short(h.Dirs[0])
	}
	name := h.Name
	if h.Default {
		// The machine a new session opens on, which is the one fact about the
		// list that is not visible from the list.
		name += "*"
	}
	if missing := co.behind[h.Name]; len(missing) > 0 {
		// Per mount: commands there under other mounts are still correct, so the
		// row says how many it is missing rather than marking the machine broken.
		where = fmt.Sprintf("behind on %d mount(s)", len(missing))
		dot = "◐"
	}
	dot = lipgloss.NewStyle().Foreground(ui.MachineColor(h.Name)).Render(dot)
	return fmt.Sprintf("%s %-11s %s", dot, trim(name, 11), where)
}

// mark highlights the focused row. The marker is reverse video rather than a
// colour, so it survives a terminal with an unhelpful palette.
func mark(row string, w int, sel bool) string {
	if !sel {
		return row
	}
	return coSel.Render(fit(ansi.Strip(row), w))
}

func shortSource(m proto.TreeMount) string {
	if m.Owner == "pod" {
		return "local"
	}
	return m.Owner + ":" + trim(m.Source[strings.Index(m.Source, ":")+1:], 22)
}

// activityLines is the right column: the live log, newest at the bottom, which is
// where a terminal reader's eye already is.
func (co *cockpit) activityLines(w, avail int) []string {
	out := []string{coHead.Render("ACTIVITY")}
	lines := co.activity
	if co.filter != "" {
		kept := make([]string, 0, len(lines))
		for _, l := range lines {
			if strings.Contains(l, co.filter) {
				kept = append(kept, l)
			}
		}
		lines = kept
		out[0] += coDim.Render("  /" + co.filter)
	}
	// Only as much as fits, and the tail is what matters.
	if room := max(avail-1, 1); len(lines) > room {
		lines = lines[len(lines)-room:]
	}
	out = append(out, lines...)
	if len(lines) == 0 {
		out = append(out, coDim.Render("  waiting for the first command"))
	}
	return out
}
