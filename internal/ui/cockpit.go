package ui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"vibepod/internal/proto"
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
//
// It is drawn in the same language as `vp shell`: a machine has one colour
// everywhere, the accent marks where you are, and hints are dim and on the
// right.

// CockpitState is what the panes show, as the daemon last reported it.
type CockpitState struct {
	Hosts    []proto.HostInfo
	Sessions []proto.SessionInfo
	Mounts   []proto.TreeMount
	// Default is the machine a new session opens on.
	Default string
	// Behind is, per machine, the mounts its node pod does not hold yet.
	Behind map[string][]string
	Err    error
}

// Activity is one line of the log pane.
type Activity struct {
	Time    time.Time
	Machine string
	What    string
	// Running is an exec that has not exited; Code is set once it has, and nil
	// when vibepod could only observe it and never learned how it ended.
	Running bool
	Code    *int
	Dur     string
	// PID pairs an exec with its exit, so a finished command replaces its own
	// running line rather than adding a second one. Where the two events do not
	// share one, the machine and the command line do the pairing.
	PID int
}

// CockpitConfig is everything the cockpit does to the world, supplied by the
// command line, which holds the daemon connection.
type CockpitConfig struct {
	Pod     string
	Load    func() CockpitState
	Mount   func(spec string) error
	Unmount func(arg string) error
	Use     func(session, machine string) error
	// Attach is the process that takes the terminal for a session.
	Attach func(session string) *exec.Cmd
	// Follow streams activity until it fails; it is retried.
	Follow func(send func(Activity)) error
}

// RunCockpit runs the cockpit until q.
func RunCockpit(cfg CockpitConfig) error {
	in := textinput.New()
	in.Prompt = ""
	co := &cockpit{cfg: cfg, input: in, status: "pod " + cfg.Pod + " is up"}
	p := tea.NewProgram(co)
	go func() {
		for {
			_ = cfg.Follow(func(a Activity) { p.Send(activityMsg(a)) })
			time.Sleep(time.Second)
		}
	}()
	_, err := p.Run()
	return err
}

type cockpit struct {
	cfg CockpitConfig
	st  CockpitState

	activity []Activity
	filter   string
	status   string
	focus    int // 0: machines, 1: sessions
	selHost  int
	selSess  int
	w, h     int
	spin     int
	spinning bool
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
	stateMsg    CockpitState
	activityMsg Activity
	statusMsg   string
	refreshTick struct{}
	spinTick    struct{}
	attachedMsg struct {
		id  string
		err error
	}
)

func (co *cockpit) Init() tea.Cmd { return tea.Batch(co.refresh(), refreshEvery()) }

func refreshEvery() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return refreshTick{} })
}

func (co *cockpit) refresh() tea.Cmd {
	load := co.cfg.Load
	return func() tea.Msg { return stateMsg(load()) }
}

func (co *cockpit) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		co.w, co.h = msg.Width, msg.Height
	case stateMsg:
		if msg.Err != nil {
			co.status = "vibepod: " + msg.Err.Error()
			break
		}
		sessions := co.st.Sessions
		co.st = CockpitState(msg)
		if co.st.Sessions == nil {
			co.st.Sessions = sessions
		}
		co.selHost = clamp(co.selHost, len(co.st.Hosts))
		co.selSess = clamp(co.selSess, len(co.st.Sessions))
	case refreshTick:
		return co, tea.Batch(co.refresh(), refreshEvery())
	case activityMsg:
		co.add(Activity(msg))
		return co, co.spinner()
	case spinTick:
		co.spinning = false
		co.spin++
		return co, co.spinner()
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

// add puts a line in the log, replacing the running line of the command it
// finishes.
func (co *cockpit) add(a Activity) {
	if !a.Running {
		match := -1
		for i := len(co.activity) - 1; i >= 0; i-- {
			o := co.activity[i]
			if !o.Running || o.Machine != a.Machine {
				continue
			}
			if a.PID != 0 && o.PID == a.PID {
				match = i
				break
			}
			if match < 0 && o.What == a.What {
				match = i
			}
		}
		if match >= 0 {
			co.activity[match] = a
			return
		}
	}
	co.activity = append(co.activity, a)
	if len(co.activity) > 500 {
		co.activity = co.activity[len(co.activity)-500:]
	}
}

// spinner keeps the running lines turning, and stops when none are.
func (co *cockpit) spinner() tea.Cmd {
	if co.spinning {
		return nil
	}
	for _, a := range co.activity {
		if a.Running {
			co.spinning = true
			return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return spinTick{} })
		}
	}
	return nil
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
	case "right", "l":
		co.focus = 1
	case "left", "h":
		co.focus = 0
	case "tab":
		co.focus = 1 - co.focus
	case "enter":
		return co.attach()
	case "m":
		return co.ask("mount (host:/path)", false, func(line string) tea.Cmd {
			return co.do("mount "+line, func() error { return co.cfg.Mount(line) })
		})
	case "u":
		return co.ask("unmount (path or @machine)", false, func(line string) tea.Cmd {
			return co.do("unmount "+line, func() error { return co.cfg.Unmount(line) })
		})
	case "b":
		return co.ask("backend for this session", false, co.setBackend)
	case "/":
		return co.ask("filter", true, func(line string) tea.Cmd {
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
	if label == "filter" {
		co.input.SetValue(co.filter)
		co.input.CursorEnd()
	}
	return co.input.Focus()
}

func (co *cockpit) move(d int) {
	if co.focus == 0 {
		co.selHost = clamp(co.selHost+d, len(co.st.Hosts))
	} else {
		co.selSess = clamp(co.selSess+d, len(co.st.Sessions))
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

func (co *cockpit) selected() (proto.SessionInfo, bool) {
	if co.selSess < len(co.st.Sessions) {
		return co.st.Sessions[co.selSess], true
	}
	return proto.SessionInfo{}, false
}

// do runs one control action and puts how it went on the status line.
func (co *cockpit) do(what string, f func() error) tea.Cmd {
	co.status = what + "…"
	return func() tea.Msg {
		if err := f(); err != nil {
			return statusMsg(what + ": " + err.Error())
		}
		return statusMsg(what + " — done")
	}
}

// setBackend moves the focused session, which is the key the whole model turns
// on: the machine is chosen, and this is where a human chooses it.
func (co *cockpit) setBackend(target string) tea.Cmd {
	s, ok := co.selected()
	if !ok {
		co.status = "no session selected; ⇥ to the session list first"
		return nil
	}
	target = strings.TrimPrefix(target, "@")
	use := co.cfg.Use
	return func() tea.Msg {
		if err := use(s.ID, target); err != nil {
			return statusMsg(err.Error())
		}
		return statusMsg("session " + s.ID + " is now on " + target)
	}
}

func (co *cockpit) attach() tea.Cmd {
	s, ok := co.selected()
	if !ok {
		co.status = "nothing to attach to — `vp shell` opens a new terminal"
		return nil
	}
	return tea.ExecProcess(co.cfg.Attach(s.ID), func(err error) tea.Msg {
		return attachedMsg{s.ID, err}
	})
}

// afterAttach says how the session was left, which only the daemon still
// knows: `vp attach` has exited either way.
func (co *cockpit) afterAttach(msg attachedMsg) tea.Cmd {
	load := co.cfg.Load
	return func() tea.Msg {
		for _, s := range load().Sessions {
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

// Drawing.

var (
	stBorder = lipgloss.NewStyle().Foreground(colDim)
	stHead   = lipgloss.NewStyle().Bold(true)
	stFocus  = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
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
	if h < 8 || w < 50 {
		// Too small to divide. Say so rather than drawing something broken.
		return "vibepod: the cockpit needs at least 50x8"
	}
	inner := w - 2
	left := min(inner*2/5, 44)
	right := inner - left - 1
	body := h - 3 // the frame's two edges, and the status line

	l, r := co.machinePane(left, body), co.activityPane(right, body)
	var b strings.Builder
	b.WriteString(co.topEdge(w) + "\n")
	side := stBorder.Render("│")
	for i := 0; i < body; i++ {
		a, c := "", ""
		if i < len(l) {
			a = l[i]
		}
		if i < len(r) {
			c = r[i]
		}
		b.WriteString(side + fit(a, left) + side + fit(c, right) + side + "\n")
	}
	b.WriteString(co.bottomEdge(w) + "\n")
	b.WriteString(co.statusLine(w))
	return b.String()
}

// topEdge is the frame's top border with the pod's name set into it.
func (co *cockpit) topEdge(w int) string {
	title := " " + stBold.Render("vibepod") + stDim.Render(" · ") +
		stAccent.Render(co.cfg.Pod) + " "
	fill := max(w-3-lipgloss.Width(title), 0)
	return stBorder.Render("╭─") + title + stBorder.Render(strings.Repeat("─", fill)+"╮")
}

// bottomEdge carries the last thing the cockpit has to say, set into the
// frame the way the pod's name is set into the top.
func (co *cockpit) bottomEdge(w int) string {
	if co.status == "" {
		return stBorder.Render("╰" + strings.Repeat("─", w-2) + "╯")
	}
	msg := " " + ansi.Truncate(co.status, max(w-8, 10), "…") + " "
	fill := max(w-3-lipgloss.Width(msg), 0)
	return stBorder.Render("╰─") + msg + stBorder.Render(strings.Repeat("─", fill)+"╯")
}

// fit makes s exactly w columns wide, cutting or padding.
func fit(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	if n := lipgloss.Width(s); n < w {
		s += strings.Repeat(" ", w-n)
	}
	return s
}

func (co *cockpit) header(title string, pane int) string {
	if co.focus == pane {
		return " " + stFocus.Render(title)
	}
	return " " + stHead.Render(title)
}

// row is one line of a list: the accent marker on the selection in the focused
// pane, a dim one where the other pane's selection is, nothing elsewhere.
func (co *cockpit) row(s string, pane int, sel bool) string {
	switch {
	case sel && co.focus == pane:
		return " " + stAccent.Render("❯") + " " + s
	case sel:
		return " " + stDim.Render("›") + " " + s
	}
	return "   " + s
}

func paint(machine string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(MachineColor(machine))
}

// machinePane is the left column: machines, sessions, mounts. All three are
// lists of things you act on, so they share a column and the focus moves
// between them.
//
// The budget matters more than it sounds. A well-used ssh config has thirty
// hosts, and listing every machine this pod *could* mount would push the
// sessions — the thing you came to look at — off the bottom of the screen. So
// mounted machines are always shown, and the unmounted ones take whatever room
// is left over after the sessions and the mounts have had theirs.
func (co *cockpit) machinePane(w, avail int) []string {
	st := co.st
	var mounted, unmounted []int
	nameW := 6
	for i, h := range st.Hosts {
		if h.Mounted {
			mounted = append(mounted, i)
		} else {
			unmounted = append(unmounted, i)
		}
		nameW = max(nameW, min(len(h.Name)+1, 16))
	}

	sessions := []string{"", co.header("SESSIONS", 1)}
	if len(st.Sessions) == 0 {
		sessions = append(sessions, "   "+stDim.Render("none — `vp shell` opens one"))
	}
	for i, s := range st.Sessions {
		kind := s.Kind
		if kind == "" {
			kind = "session"
		}
		line := fmt.Sprintf("%-3s %-7s %s %s", s.ID, kind, stDim.Render("▸"),
			paint(s.Backend).Render(s.Backend))
		sessions = append(sessions, co.row(line, 1, i == co.selSess))
	}
	mounts := []string{"", " " + stHead.Render("MOUNTS")}
	for _, m := range st.Mounts {
		owner := m.Owner
		if owner == "" || owner == "pod" {
			owner = "local"
		}
		at := shortHome(m.At)
		room := max(w-5-lipgloss.Width(owner), 8)
		mounts = append(mounts, "   "+spread(ansi.Truncate(at, room, "…"),
			paint(m.Owner).Render(owner), w-4))
	}

	out := []string{co.header("MACHINES", 0)}
	room := avail - 1 - len(sessions) - len(mounts) - len(mounted)
	shown := min(len(unmounted), max(room, 0))
	host := func(i int) {
		out = append(out, co.row(co.hostRow(st.Hosts[i], nameW), 0, i == co.selHost))
	}
	for _, i := range mounted {
		host(i)
	}
	for _, i := range unmounted[:shown] {
		host(i)
	}
	if more := len(unmounted) - shown; more > 0 {
		out = append(out, "   "+stDim.Render(fmt.Sprintf("…%d more this pod could mount", more)))
	}
	out = append(out, sessions...)
	// A section header with nothing under it is worse than no section, and on a
	// short terminal the mounts are the part you can go and read elsewhere.
	if avail-len(out) >= len(mounts) || avail-len(out) > 2 {
		out = append(out, mounts...)
	}
	return out
}

func (co *cockpit) hostRow(h proto.HostInfo, nameW int) string {
	dot := "●"
	switch {
	case h.Local:
	case !h.Mounted:
		dot = "○"
	case !h.Connected:
		dot = "◌"
	}
	where := stDim.Render("local")
	switch {
	case !h.Mounted:
		where = stDim.Render("vp mount " + h.Name + ":/path")
	case len(h.Dirs) > 0:
		where = shortHome(h.Dirs[0])
	}
	name := h.Name
	if h.Default {
		// The machine a new session opens on, which is the one fact about the
		// list that is not visible from the list.
		name += "*"
	}
	if missing := co.st.Behind[h.Name]; len(missing) > 0 {
		// Per mount: commands there under other mounts are still correct, so the
		// row says how many it is missing rather than marking the machine broken.
		where = stWarn.Render(fmt.Sprintf("behind on %d mount(s)", len(missing)))
		dot = "◐"
	}
	name = ansi.Truncate(name, nameW, "…")
	return paint(h.Name).Render(dot) + " " + name +
		strings.Repeat(" ", max(nameW-lipgloss.Width(name), 0)) + " " + where
}

// activityPane is the right column: the live log, newest at the bottom, which
// is where a terminal reader's eye already is. Each line carries its machine's
// bar, the same colour its blocks have in `vp shell`.
func (co *cockpit) activityPane(w, avail int) []string {
	head := " " + stHead.Render("ACTIVITY")
	lines := co.activity
	if co.filter != "" {
		kept := make([]Activity, 0, len(lines))
		for _, a := range lines {
			if strings.Contains(a.Machine+" "+a.What, co.filter) {
				kept = append(kept, a)
			}
		}
		lines = kept
		head += stDim.Render("  /" + co.filter)
	}
	out := []string{head}
	if room := max(avail-1, 1); len(lines) > room {
		lines = lines[len(lines)-room:]
	}
	if len(lines) == 0 {
		return append(out, "   "+stDim.Render("waiting for the first command"))
	}
	mw := 4
	for _, a := range lines {
		mw = max(mw, min(len(a.Machine), 14))
	}
	for _, a := range lines {
		out = append(out, co.activityRow(a, w, mw))
	}
	return out
}

func (co *cockpit) activityRow(a Activity, w, mw int) string {
	machine := a.Machine
	if machine == "" {
		machine = "—"
	}
	left := " " + bar(a.Machine) + stDim.Render(a.Time.Format("15:04")) + " " +
		paint(a.Machine).Render(fmt.Sprintf("%-*s", mw, ansi.Truncate(machine, mw, "…"))) + " "
	var right string
	switch {
	case a.Running:
		right = stAccent.Render(spinner[co.spin%len(spinner)])
	case a.Code == nil:
		right = stDim.Render(a.Dur)
	case *a.Code == 0:
		right = stOK.Render("✓") + stDim.Render(" "+a.Dur)
	default:
		right = stFail.Render(fmt.Sprintf("✗ %d", *a.Code)) + stDim.Render(" "+a.Dur)
	}
	right = strings.TrimRight(right, " ") + " "
	room := w - lipgloss.Width(left) - lipgloss.Width(right) - 1
	what := ansi.Truncate(a.What, max(room, 4), "…")
	return spread(left+what, right, w)
}

// statusLine is below the frame, laid out like `vp shell`'s: where a key
// would act on the left, what the keys are on the right — or, while a
// question is open, the question.
func (co *cockpit) statusLine(w int) string {
	if q := co.asking; q != nil {
		return fit(" "+stAccent.Render("❯ "+q.label+": ")+co.input.View(), w)
	}
	machine := co.st.Default
	who := stDim.Render("new sessions")
	if s, ok := co.selected(); ok {
		machine = s.Backend
		who = stDim.Render("session " + s.ID)
	}
	if machine == "" {
		machine = "—"
	}
	left := " " + paint(machine).Bold(true).Render(machine) + "  " + who
	keys := stDim.Render("m mount · u unmount · b backend · ⏎ attach · ⇥ pane · / filter · q quit ")
	if lipgloss.Width(left)+lipgloss.Width(keys)+2 > w {
		keys = stDim.Render("⏎ attach · q quit ")
	}
	return spread(left, keys, w)
}

func shortHome(p string) string {
	home, _ := os.UserHomeDir()
	return tilde(p, home)
}
