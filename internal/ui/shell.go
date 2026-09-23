package ui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"vibepod/internal/proto"
)

// `vp shell`: a line to type at the bottom, and every command's output above it
// as a block — which machine, which directory, how it ended.
//
// Underneath is the same shell the raw terminal had: the daemon's pty, a login
// shell in the pod or over ssh, with its cwd, its jobs and its environment
// carried from one command to the next. This only reads the shell's output
// differently. The hooks mark where each command's output begins and ends;
// between them the output goes through a terminal emulator, so a progress bar,
// a Python REPL, a password prompt or Claude Code's own inline interface all
// work, with every key going straight to the program. A program that takes the
// whole screen gets the whole terminal, and the blocks come back when it
// leaves.
//
// Finished blocks are printed into the terminal's own scrollback, the way
// Claude Code and Codex do it: the terminal scrolls, searches and copies them,
// and they are still there after you quit. Only the running block and the
// input line are redrawn.

// ShellConfig is what RunShell needs from the command line.
type ShellConfig struct {
	Conn *proto.Conn
	// Out is the read end of the pipe the daemon writes the session's output to.
	Out     *os.File
	Pod     string
	Session string
	// History is the file ↑ reads from and every submitted line is added to.
	History string
	// Rows and Cols are the terminal's size at the start: the replayed
	// scrollback can arrive before Bubble Tea has measured the window.
	Rows, Cols int
}

// ShellResult is how a `vp shell` ended.
type ShellResult struct {
	Code     int
	Detached bool
	Err      error
}

// EmuSize is the terminal a command sees for a window of this size: the width
// less the block's bar and a column to spare, the height less the input box and status line. The pty
// is kept at this size so that what a program lays out fits where it is drawn.
func EmuSize(rows, cols int) (int, int) {
	return max(rows-chromeRows, 3), max(cols-3, 20)
}

// chromeRows is what the live block cannot use: a blank line, the input box's
// three rows and the status line.
const chromeRows = 5

type phase int

const (
	phBoot phase = iota // waiting for the shell's first marked prompt
	phIdle              // at the prompt: keys edit the input line
	phSent              // a line was sent; waiting for the shell to run it
	phRun               // a command is running: keys go to it
	phHand              // a full-screen program has the terminal
	phRaw               // the hooks never took: the terminal is the shell's
)

type (
	streamMsg    []Event
	handStartMsg struct{ h *handover }
	handDoneMsg  struct{ err error }
	endMsg       struct {
		code     int
		detached bool
		err      error
	}
	sentTimeoutMsg struct{ seq int }
	bootTimeoutMsg struct{ seq int }
	tickMsg        struct{}
)

// RunShell runs the interface until the session ends or is detached from. The
// session must already be attached, framed, with its output on cfg.Out.
func RunShell(cfg ShellConfig) ShellResult {
	m := newShell(cfg)
	p := tea.NewProgram(m)
	m.pr = newPrinter(p)
	m.st = &stream{p: p}
	go m.pr.run()
	go m.st.run(cfg.Out)
	go func() {
		for {
			r, fds, err := cfg.Conn.Recv()
			for _, fd := range fds {
				_ = os.NewFile(uintptr(fd), "").Close()
			}
			if err != nil {
				p.Send(endMsg{err: err})
				return
			}
			switch r.Op {
			case proto.OpExit:
				p.Send(endMsg{code: r.Code})
				return
			case proto.OpDetach:
				p.Send(endMsg{detached: true})
				return
			case proto.OpErr:
				p.Send(endMsg{err: fmt.Errorf("%s", r.Err)})
				return
			}
		}
	}()
	if _, err := p.Run(); err != nil && m.res.Err == nil {
		m.res.Err = err
	}
	return m.res
}

type shellModel struct {
	cfg ShellConfig
	st  *stream
	pr  *printer

	w, h int
	in   textarea.Model
	hist []string
	// histAt is where ↑/↓ is in hist; len(hist) is the line being written, kept
	// in draft while you look at older ones.
	histAt int
	draft  string

	ph       phase
	backend  string
	cwd      string
	home     string
	booted   map[string]bool
	lastCode int
	// quiet is set while the daemon replays a shell's scrollback: the markers in
	// it rebuild the state — at a prompt, or inside a command — without
	// printing blocks that were already printed once.
	quiet   bool
	sawMark bool
	// echo is what the shell drew between its prompt and a command's output:
	// the line being edited. Discarded, except when replaying, where it is the
	// only record of what the running command was.
	echo    []byte
	sentSeq int
	bootSeq int
	ticking bool
	spin    int

	// The running block.
	cmd       string
	started   time.Time
	blockOn   string
	blockCwd  string
	emu       *vt.Emulator
	curHidden bool

	quitting bool
	res      ShellResult
}

func newShell(cfg ShellConfig) *shellModel {
	in := textarea.New()
	in.ShowLineNumbers = false
	in.Prompt = ""
	in.SetPromptFunc(2, func(pi textarea.PromptInfo) string {
		if pi.LineNumber == 0 {
			return stAccent.Render("❯ ")
		}
		return stDim.Render("· ")
	})
	in.DynamicHeight = true
	in.MinHeight = 1
	in.MaxHeight = 8
	in.SetVirtualCursor(false)
	st := textarea.DefaultStyles(true)
	for _, s := range []*textarea.StyleState{&st.Focused, &st.Blurred} {
		s.Base = lipgloss.NewStyle()
		s.CursorLine = lipgloss.NewStyle()
		s.Text = lipgloss.NewStyle()
		s.Prompt = lipgloss.NewStyle()
		s.Placeholder = stDim
		s.EndOfBuffer = lipgloss.NewStyle()
	}
	in.SetStyles(st)
	// Enter runs the line; a newline inside it is alt+enter or shift+enter, the
	// way every chat box does it.
	in.KeyMap.InsertNewline.SetKeys("alt+enter", "shift+enter", "ctrl+j")
	in.Focus()
	m := &shellModel{cfg: cfg, in: in, booted: map[string]bool{},
		hist: loadHistory(cfg.History), w: cfg.Cols, h: cfg.Rows}
	m.in.SetWidth(max(m.w-4, 10))
	m.histAt = len(m.hist)
	return m
}

func (m *shellModel) Init() tea.Cmd { return nil }

func (m *shellModel) send(b []byte) {
	if len(b) > 0 {
		_ = m.cfg.Conn.Send(&proto.Msg{Op: proto.OpInput, Data: b})
	}
}

func (m *shellModel) winch(rows, cols int) {
	_ = m.cfg.Conn.Send(&proto.Msg{Op: proto.OpWinch, Rows: rows, Cols: cols})
}

func (m *shellModel) print(s string) {
	if !m.quiet {
		m.pr.add(s)
	}
}

func (m *shellModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.in.SetWidth(max(m.w-4, 10))
		rows, cols := EmuSize(m.h, m.w)
		if m.emu != nil {
			m.emu.Resize(cols, rows)
		}
		if m.ph != phHand && m.ph != phRaw {
			m.winch(rows, cols)
		}
		return m, nil

	case streamMsg:
		var cmds []tea.Cmd
		for _, ev := range msg {
			cmds = append(cmds, m.handle(ev))
		}
		m.drain()
		return m, tea.Batch(cmds...)

	case handStartMsg:
		m.drain()
		m.ph = phHand
		msg.h.setConn(m.cfg.Conn)
		return m, tea.Exec(msg.h, func(err error) tea.Msg { return handDoneMsg{err} })

	case handDoneMsg:
		if m.ph == phHand {
			m.ph = phRun
		}
		if m.ph != phRaw {
			rows, cols := EmuSize(m.h, m.w)
			m.winch(rows, cols)
		}
		return m, m.tick()

	case endMsg:
		m.res = ShellResult{Code: msg.code, Detached: msg.detached, Err: msg.err}
		if m.emu != nil {
			m.finish(msg.code, !msg.detached)
		}
		m.quitting = true
		pr := m.pr
		return m, func() tea.Msg { pr.wait(); return tea.QuitMsg{} }

	case sentTimeoutMsg:
		// No output marker, but the shell did something with the line: asked for
		// its continuation, offered a correction, or is a bash too old to say. Show
		// it, and let keys reach it.
		if m.ph == phSent && msg.seq == m.sentSeq {
			echo := m.echo
			m.startBlock()
			if i := bytes.IndexAny(echo, "\r\n"); i >= 0 && len(echo) > 0 {
				_, _ = m.emu.Write(echo)
			}
			m.echo = nil
			return m, m.tick()
		}
		return m, nil

	case bootTimeoutMsg:
		if m.ph == phBoot && msg.seq == m.bootSeq && !m.sawMark {
			m.ph = phRaw
			m.print(stWarn.Render("vibepod: this shell did not take vp shell's hooks "+
				"(only bash and zsh do) — it is yours raw now; ctrl+\\ detaches") + "\n")
			h := m.st.goRaw(m.cfg.Conn)
			if m.h > 0 {
				m.winch(m.h, m.w)
			}
			return m, tea.Exec(h, func(err error) tea.Msg { return handDoneMsg{err} })
		}
		return m, nil

	case tickMsg:
		m.ticking = false
		if m.ph == phRun || m.ph == phSent {
			m.spin++
			return m, m.tick()
		}
		return m, nil

	case tea.PasteMsg:
		if m.ph == phRun && m.emu != nil {
			m.emu.Paste(msg.Content)
			return m, nil
		}

	case tea.KeyPressMsg:
		return m.key(msg)
	}

	if m.ph == phIdle {
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *shellModel) tick() tea.Cmd {
	if m.ticking {
		return nil
	}
	m.ticking = true
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *shellModel) key(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+\\" {
		_ = m.cfg.Conn.Send(&proto.Msg{Op: proto.OpDetach})
		return m, nil
	}
	switch m.ph {
	case phRun:
		if m.emu != nil {
			m.emu.SendKey(uv.KeyPressEvent(uv.Key(k)))
		}
		return m, nil
	case phSent:
		// The shell has the line but has not started it. What is typed now is
		// the next line, which is where it would go in a terminal too.
		switch k.String() {
		case "ctrl+c":
			m.send([]byte{3})
			return m, nil
		case "enter":
			return m, nil
		}
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(k)
		return m, cmd
	case phIdle:
	default:
		return m, nil
	}

	switch k.String() {
	case "enter":
		return m.submit()
	case "ctrl+c":
		m.in.Reset()
		m.histAt = len(m.hist)
		return m, nil
	case "ctrl+d":
		if m.in.Value() == "" {
			m.send([]byte("exit\r"))
			m.ph = phSent
			m.sentSeq++
			return m, nil
		}
	case "ctrl+l":
		return m, tea.ClearScreen
	case "up":
		if m.in.Line() == 0 && m.histAt > 0 {
			if m.histAt == len(m.hist) {
				m.draft = m.in.Value()
			}
			m.histAt--
			m.in.SetValue(m.hist[m.histAt])
			return m, nil
		}
	case "down":
		if m.in.Line() == m.in.LineCount()-1 && m.histAt < len(m.hist) {
			m.histAt++
			if m.histAt == len(m.hist) {
				m.in.SetValue(m.draft)
			} else {
				m.in.SetValue(m.hist[m.histAt])
			}
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(k)
	return m, cmd
}

func (m *shellModel) submit() (tea.Model, tea.Cmd) {
	line := m.in.Value()
	if strings.TrimSpace(line) == "" {
		return m, nil
	}
	if len(m.hist) == 0 || m.hist[len(m.hist)-1] != line {
		m.hist = append(m.hist, line)
		appendHistory(m.cfg.History, line)
	}
	m.histAt, m.draft = len(m.hist), ""
	m.in.Reset()
	m.cmd = line
	m.echo = nil
	m.ph = phSent
	m.sentSeq++
	seq := m.sentSeq
	m.send([]byte(line + "\r"))
	return m, tea.Batch(m.tick(), tea.Tick(400*time.Millisecond,
		func(time.Time) tea.Msg { return sentTimeoutMsg{seq} }))
}

// handle advances the state machine by one event.
func (m *shellModel) handle(ev Event) tea.Cmd {
	switch ev.Kind {
	case Text:
		// A block is running for as long as it has an emulator, whatever the
		// phase says: output that follows a full-screen program's exit can
		// arrive before the interface has taken the terminal back.
		switch {
		case m.emu != nil:
			_, _ = m.emu.Write(ev.Data)
		case m.ph == phSent || m.ph == phIdle:
			m.echo = append(m.echo, ev.Data...)
		}

	case Backend:
		if m.emu != nil {
			m.finish(m.lastCode, false)
		}
		if ev.Ended != "" {
			m.print(stDim.Render(fmt.Sprintf("the shell on %s ended; back on %s",
				ev.Ended, ev.Backend)) + "\n")
		} else if m.backend != "" && ev.Backend != m.backend {
			m.print(bar(ev.Backend) + stDim.Render("now on ") +
				lipgloss.NewStyle().Foreground(MachineColor(ev.Backend)).
					Render(ev.Backend) + "\n")
		}
		m.backend = ev.Backend
		m.quiet, m.sawMark = true, false
		m.ph, m.echo = phBoot, nil

	case Replayed:
		m.quiet = false
		if !m.sawMark {
			// A shell never seen with markers: new, or one whose hooks scrolled out
			// of its ring. Typing them is safe either way, because a shell with no
			// marked prompt in all its scrollback is not running anything of ours.
			m.booted[m.backend] = true
			m.send([]byte(Hooks()))
			m.bootSeq++
			seq := m.bootSeq
			return tea.Tick(8*time.Second, func(time.Time) tea.Msg { return bootTimeoutMsg{seq} })
		}
		if m.emu != nil {
			m.print(m.header(stDim.Render(" (was running)")))
			m.drain()
			return m.tick()
		}

	case Cwd:
		m.cwd, m.home = ev.Cwd, ev.Home

	case Prompt:
		m.sawMark = true
		if m.emu != nil {
			m.finish(m.lastCode, true)
		}

	case Input:
		m.sawMark = true
		m.booted[m.backend] = true
		if m.emu != nil {
			m.finish(m.lastCode, true)
		}
		if m.ph != phSent {
			m.ph = phIdle
			m.echo = nil
		}

	case Exec:
		m.sawMark = true
		// A line this interface did not send — replayed, or typed ahead into the
		// previous command and read by the shell after it — is named by what the
		// shell drew for it.
		if m.quiet || m.ph != phSent {
			m.cmd = lastLine(m.echo)
		}
		if m.emu != nil {
			m.finish(m.lastCode, false)
		}
		m.echo = nil
		m.startBlock()
		return m.tick()

	case Done:
		m.sawMark = true
		m.lastCode = ev.Code
		if m.emu != nil {
			m.finish(ev.Code, true)
		}
		if m.ph != phSent {
			m.ph = phIdle
		}
	}
	return nil
}

// lastLine recovers a command line from what the shell drew while it was
// being edited. Line editors redraw — backspaces, cursor moves, a completion
// menu wiped away — so the bytes are put through a terminal and what is left
// on it is the line.
func lastLine(echo []byte) string {
	e := vt.NewEmulator(1024, 32)
	go func() { _, _ = io.Copy(io.Discard, e) }()
	defer e.Close()
	_, _ = e.Write(echo)
	var lines []string
	for _, l := range strings.Split(e.String(), "\n") {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		return "…"
	}
	return strings.Join(lines, "\n")
}

func (m *shellModel) startBlock() {
	rows, cols := EmuSize(m.h, m.w)
	e := vt.NewEmulator(cols, rows)
	e.SetScrollbackSize(1 << 20)
	e.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(v bool) { m.curHidden = !v },
	})
	m.curHidden = false
	// What the emulator says back — keys encoded for the program's modes, and
	// answers to its queries — is the program's input.
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := e.Read(b)
			if n > 0 {
				m.send(append([]byte(nil), b[:n]...))
			}
			if err != nil {
				return
			}
		}
	}()
	m.emu = e
	m.started = time.Now()
	m.blockOn, m.blockCwd = m.backend, m.cwd
	m.ph = phRun
	m.print(m.header(""))
}

func (m *shellModel) header(extra string) string {
	left := bar(m.blockOn) + stBold.Render("$ "+strings.ReplaceAll(m.cmd, "\n", " ⏎ ")) + extra
	right := stDim.Render(tilde(m.blockCwd, m.home) + " · " + m.blockOn)
	// One column short of the width: a line that fills the last column leaves
	// the cursor in the terminal's pending-wrap state, and what is printed next
	// can eat its last character.
	return spread(left, right, max(m.w-1, 20))
}

// drain moves lines the running program scrolled off its screen into the
// terminal's scrollback, where they will not change again.
func (m *shellModel) drain() {
	if m.emu == nil || m.quiet {
		return
	}
	sb := m.emu.Scrollback()
	if sb == nil || sb.Len() == 0 {
		return
	}
	var b strings.Builder
	for i, l := range sb.Lines() {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(bar(m.blockOn) + l.Render())
	}
	m.emu.ClearScrollback()
	m.print(b.String())
}

// screenRows is the running program's screen, from the top down to the last
// line that has anything on it or the cursor, whichever is lower.
func (m *shellModel) screenRows(withCursor bool) []string {
	if m.emu == nil {
		return nil
	}
	rows := strings.Split(m.emu.Render(), "\n")
	last := -1
	for i, r := range rows {
		if strings.TrimSpace(ansi.Strip(r)) != "" {
			last = i
		}
	}
	if withCursor && !m.curHidden {
		last = max(last, m.emu.CursorPosition().Y)
	}
	if last+1 < len(rows) {
		rows = rows[:last+1]
	}
	return rows
}

// finish closes the running block: what is left on its screen goes to the
// scrollback, followed by how it ended when that is worth a line.
func (m *shellModel) finish(code int, ended bool) {
	m.drain()
	var b strings.Builder
	for _, r := range m.screenRows(false) {
		b.WriteString(bar(m.blockOn) + r + "\n")
	}
	took := time.Since(m.started)
	switch {
	case !ended:
	case code != 0:
		b.WriteString(bar(m.blockOn) + stFail.Render(fmt.Sprintf("✗ exit %d", code)) +
			stDim.Render(" · "+round(took)) + "\n")
	case took >= 2*time.Second:
		b.WriteString(bar(m.blockOn) + stOK.Render("✓") + stDim.Render(" "+round(took)) + "\n")
	}
	m.print(b.String() + "\n")
	_ = m.emu.Close()
	m.emu = nil
	if m.ph == phRun || m.ph == phHand {
		m.ph = phIdle
	}
}

func round(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Truncate(time.Second).String()
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m *shellModel) View() tea.View {
	if m.quitting || m.w == 0 || m.ph == phHand || m.ph == phRaw {
		return tea.NewView("")
	}
	var lines []string
	var cur *tea.Cursor

	if m.ph == phRun {
		rows := m.screenRows(true)
		for _, r := range rows {
			lines = append(lines, bar(m.blockOn)+r)
		}
		if !m.curHidden {
			p := m.emu.CursorPosition()
			if p.Y < len(rows) {
				cur = tea.NewCursor(p.X+2, p.Y)
			}
		}
	}
	lines = append(lines, "")

	border := colAccent
	if m.ph != phIdle {
		border = colDim
		m.in.Placeholder = ""
	} else {
		m.in.Placeholder = "run on " + m.backendName()
	}
	var body string
	switch m.ph {
	case phBoot:
		body = stDim.Render(spinner[m.spin%len(spinner)] + " starting the shell on " + m.backendName() + "…")
	case phRun:
		body = stDim.Render("keys go to the running command")
	default:
		body = m.in.View()
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(border).Padding(0, 1).Width(m.w).Render(body)
	top := len(lines)
	lines = append(lines, strings.Split(box, "\n")...)
	if m.ph == phIdle {
		if c := m.in.Cursor(); c != nil {
			c.Position.X += 2
			c.Position.Y += top + 1
			cur = c
		}
	}
	lines = append(lines, m.status())

	v := tea.NewView(strings.Join(lines, "\n"))
	v.Cursor = cur
	return v
}

func (m *shellModel) backendName() string {
	if m.backend == "" {
		return "the pod"
	}
	return m.backend
}

func (m *shellModel) status() string {
	on := lipgloss.NewStyle().Foreground(MachineColor(m.backend)).Bold(true).Render(m.backendName())
	left := " " + on + stDim.Render("  "+tilde(m.cwd, m.home))
	if m.lastCode != 0 && m.ph == phIdle {
		left += stFail.Render(fmt.Sprintf("  ✗ %d", m.lastCode))
	}
	var right string
	switch m.ph {
	case phRun, phSent:
		right = stAccent.Render(spinner[m.spin%len(spinner)]) +
			stDim.Render(fmt.Sprintf(" %s · ctrl+c interrupt · ctrl+\\ detach ", round(time.Since(m.started))))
	default:
		right = stDim.Render("↑ history · alt+⏎ newline · ctrl+d exit · ctrl+\\ detach ")
	}
	return spread(left, right, m.w)
}

// stream reads the session's output and splits it, handing a full-screen
// program the terminal directly: the moment it switches to the alternate
// screen, before any of what it draws there can reach the emulator.
type stream struct {
	p      *tea.Program
	parser Parser
	inCmd  bool

	mu   sync.Mutex
	hand *handover
}

func (s *stream) run(r io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.feed(buf[:n])
		}
		if err != nil {
			s.endHand(true)
			return
		}
	}
}

func (s *stream) current() *handover {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hand
}

func (s *stream) endHand(all bool) {
	s.mu.Lock()
	h := s.hand
	if h != nil && (all || !h.forever) {
		s.hand = nil
	} else {
		h = nil
	}
	s.mu.Unlock()
	if h != nil {
		h.end()
	}
}

// goRaw gives the terminal to the shell for good.
func (s *stream) goRaw(c *proto.Conn) *handover {
	h := newHandover(c, true)
	s.mu.Lock()
	s.hand = h
	s.mu.Unlock()
	return h
}

var (
	altOn  = [][]byte{[]byte("\x1b[?1049h"), []byte("\x1b[?1047h"), []byte("\x1b[?47h")}
	altOff = [][]byte{[]byte("\x1b[?1049l"), []byte("\x1b[?1047l"), []byte("\x1b[?47l")}
)

// find returns where the first of seqs starts in b and where it ends.
func find(b []byte, seqs [][]byte) (int, int) {
	at, end := -1, -1
	for _, q := range seqs {
		if i := bytes.Index(b, q); i >= 0 && (at < 0 || i < at) {
			at, end = i, i+len(q)
		}
	}
	return at, end
}

func (s *stream) feed(b []byte) {
	var batch []Event
	flush := func() {
		if len(batch) > 0 {
			s.p.Send(streamMsg(batch))
			batch = nil
		}
	}
	for _, ev := range s.parser.Feed(b) {
		if h := s.current(); h != nil {
			if h.forever {
				if ev.Kind == Text {
					h.data <- ev.Data
				}
				continue
			}
			if ev.Kind == Text {
				if _, end := find(ev.Data, altOff); end >= 0 {
					h.data <- ev.Data[:end]
					s.endHand(false)
					if rest := ev.Data[end:]; len(rest) > 0 {
						batch = append(batch, Event{Kind: Text, Data: rest})
					}
				} else {
					h.data <- ev.Data
				}
				continue
			}
			s.endHand(false)
		}
		switch ev.Kind {
		case Exec:
			s.inCmd = true
		case Done, Prompt, Backend:
			s.inCmd = false
		}
		if ev.Kind == Text && s.inCmd {
			if at, _ := find(ev.Data, altOn); at >= 0 {
				if at > 0 {
					batch = append(batch, Event{Kind: Text, Data: ev.Data[:at]})
				}
				flush()
				h := newHandover(nil, false)
				h.data <- ev.Data[at:]
				s.mu.Lock()
				s.hand = h
				s.mu.Unlock()
				s.p.Send(handStartMsg{h})
				continue
			}
		}
		batch = append(batch, ev)
	}
	flush()
}

// printer puts lines above the interface in the order they were produced.
// Bubble Tea runs each command in its own goroutine, so two prints from two
// updates would otherwise race, and a block's output could land above its
// own header.
type printer struct {
	p    *tea.Program
	mu   sync.Mutex
	cond *sync.Cond
	q    []string
	busy bool
}

func newPrinter(p *tea.Program) *printer {
	pr := &printer{p: p}
	pr.cond = sync.NewCond(&pr.mu)
	return pr
}

func (pr *printer) add(s string) {
	pr.mu.Lock()
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		s = " " // a blank line, which Println would otherwise drop
	}
	pr.q = append(pr.q, s)
	pr.mu.Unlock()
	pr.cond.Broadcast()
}

func (pr *printer) run() {
	for {
		pr.mu.Lock()
		for len(pr.q) == 0 {
			pr.cond.Wait()
		}
		items := pr.q
		pr.q = nil
		pr.busy = true
		pr.mu.Unlock()
		pr.p.Println(strings.Join(items, "\n"))
		pr.mu.Lock()
		pr.busy = false
		pr.mu.Unlock()
		pr.cond.Broadcast()
	}
}

// wait returns once everything added has been printed.
func (pr *printer) wait() {
	pr.mu.Lock()
	for len(pr.q) > 0 || pr.busy {
		pr.cond.Wait()
	}
	pr.mu.Unlock()
}
