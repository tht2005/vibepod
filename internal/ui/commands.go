package ui

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"vibepod/internal/complete"
	"vibepod/internal/proto"
)

// `vp shell`'s own commands, typed as /name.
//
// A line is one of these only when its first word is `/` and a name registered
// here, with no second slash: `/usr/bin/ls` and `/opt/x/run.sh` reach the shell
// as they always did, and so does `/bin`, which is not registered. A leading
// space sends even a registered name to the shell — which is also how a
// program called /clear at the root of a filesystem stays reachable.
//
// They exist for what the shell underneath cannot do: the shell on gpu03 has no
// `vp` on its PATH, because nothing is installed there, so moving the session,
// or clearing a screen the interface drew, has to be the interface's.

type slashCmd struct {
	name string
	args string // how its argument is written, for the menu; "" for none
	help string
	run  func(m *shellModel, args []string) tea.Cmd
	// values offers what its argument can be.
	values func(m *shellModel) ([]menuItem, error)
}

var slashCmds []*slashCmd

func init() {
	slashCmds = []*slashCmd{
		{name: "clear", help: "clear the screen and the scrollback",
			run: func(m *shellModel, _ []string) tea.Cmd { m.clearTerminal(); return nil }},
		{name: "use", args: "<machine>", help: "move this session to another machine",
			run: (*shellModel).slashUse, values: (*shellModel).machineItems},
		{name: "hosts", help: "the machines this pod knows",
			run: (*shellModel).slashHosts},
		{name: "help", help: "commands and keys",
			run: func(m *shellModel, _ []string) tea.Cmd { m.print(m.helpText()); return nil }},
		{name: "detach", help: "leave the session running and quit",
			run: func(m *shellModel, _ []string) tea.Cmd {
				_ = m.cfg.Conn.Send(&proto.Msg{Op: proto.OpDetach})
				return nil
			}},
		{name: "exit", help: "end the shell, and the session with it",
			run: func(m *shellModel, _ []string) tea.Cmd { return m.exitShell() }},
	}
}

// slashLine reports whether a line is one of these commands, and which.
func slashLine(line string) (*slashCmd, []string, bool) {
	f := strings.Fields(line)
	if !strings.HasPrefix(line, "/") || len(f) == 0 {
		return nil, nil, false
	}
	c := findSlash(strings.TrimPrefix(f[0], "/"))
	if c == nil {
		return nil, nil, false
	}
	return c, f[1:], true
}

func findSlash(name string) *slashCmd {
	for _, c := range slashCmds {
		if c.name == name {
			return c
		}
	}
	return nil
}

// menuItem is one row of the menu over the input: a command, or a completion.
type menuItem struct {
	word  string // what goes into the line
	label string // what the row says, when that differs from word
	desc  string
}

// menu is what Tab or a leading / opens. It replaces the runes of the line
// being edited from `from` to the cursor.
type menu struct {
	items []menuItem
	sel   int // -1 until something is chosen
	from  int
	slash bool // command names, following the line as it is typed
}

const menuRows = 8

// syncSlashMenu opens, narrows or closes the command menu to match the line.
func (m *shellModel) syncSlashMenu() {
	v := m.in.Value()
	if m.menu != nil && !m.menu.slash {
		return
	}
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \t\n") ||
		strings.Count(v, "/") > 1 {
		m.menu, m.menuDismissed = nil, false
		return
	}
	if m.menuDismissed {
		return
	}
	var items []menuItem
	for _, c := range slashCmds {
		if strings.HasPrefix("/"+c.name, v) {
			items = append(items, menuItem{word: "/" + c.name,
				label: strings.TrimSpace("/" + c.name + " " + c.args), desc: c.help})
		}
	}
	if len(items) == 0 {
		m.menu = nil
		return
	}
	sel := 0
	if m.menu != nil && m.menu.sel < len(items) {
		sel = max(m.menu.sel, 0)
	}
	m.menu = &menu{items: items, sel: sel, slash: true}
}

// menuKey handles a key while the menu is open. ok is false when the key is not
// the menu's, and the menu has closed so it can go where it would have gone.
func (m *shellModel) menuKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	mu := m.menu
	n := len(mu.items)
	switch k.String() {
	case "up", "ctrl+p", "shift+tab":
		mu.sel = (mu.sel - 1 + n) % n
		return nil, true
	case "down", "ctrl+n":
		mu.sel = (mu.sel + 1) % n
		return nil, true
	case "tab":
		if mu.slash {
			m.acceptSlash(mu.items[max(mu.sel, 0)])
			return nil, true
		}
		mu.sel = (mu.sel + 1) % n
		return nil, true
	case "esc":
		m.menu = nil
		m.menuDismissed = mu.slash
		return nil, true
	case "enter":
		if mu.slash {
			it := mu.items[max(mu.sel, 0)]
			if c := findSlash(strings.TrimPrefix(it.word, "/")); c != nil && c.args == "" {
				m.in.SetValue(it.word)
				m.menu = nil
				return nil, false // and run it
			}
			m.acceptSlash(it)
			return nil, true
		}
		if mu.sel >= 0 {
			m.acceptCompletion(mu.items[mu.sel].word, mu.from)
			return nil, true
		}
	}
	if !mu.slash {
		m.menu = nil
	}
	return nil, false
}

func (m *shellModel) acceptSlash(it menuItem) {
	c := findSlash(strings.TrimPrefix(it.word, "/"))
	v := it.word
	if c != nil && c.args != "" {
		v += " "
	}
	m.in.SetValue(v)
	m.menu = nil
}

// runSlash runs a /command line.
func (m *shellModel) runSlash(c *slashCmd, args []string) tea.Cmd {
	if c.args != "" && len(args) == 0 {
		m.flash = fmt.Sprintf("/%s %s", c.name, c.args)
		return nil
	}
	return c.run(m, args)
}

func (m *shellModel) slashUse(args []string) tea.Cmd {
	target := strings.TrimPrefix(args[0], "@")
	if target == "local" {
		target = "pod"
	}
	m.busy = "moving to " + target + "…"
	return tea.Batch(m.tick(), m.controlCmd(&proto.Msg{Op: proto.OpUse,
		Session: m.cfg.Session, Backend: target}, func(*proto.Msg) string { return "" }))
}

func (m *shellModel) slashHosts(_ []string) tea.Cmd {
	return m.controlCmd(&proto.Msg{Op: proto.OpHosts}, func(r *proto.Msg) string {
		var b strings.Builder
		for _, h := range r.Hosts {
			state := "not connected"
			switch {
			case h.Local:
				state = "here"
			case h.Connected:
				state = "connected"
			case !h.Mounted:
				state = "not mounted"
			}
			if h.Default {
				state += " · default"
			}
			name := lipgloss.NewStyle().Foreground(MachineColor(h.Name)).Render("@" + h.Name)
			fmt.Fprintf(&b, "%s%s%s\n", bar(h.Name), name, stDim.Render("  "+state))
			for _, d := range h.Dirs {
				fmt.Fprintf(&b, "%s  %s\n", bar(h.Name), tilde(d, m.home))
			}
		}
		return b.String()
	})
}

func (m *shellModel) machineItems() ([]menuItem, error) {
	r, err := m.control(&proto.Msg{Op: proto.OpHosts})
	if err != nil {
		return nil, err
	}
	var items []menuItem
	for _, h := range r.Hosts {
		if h.Local {
			items = append(items, menuItem{word: "pod", desc: "this machine"})
			continue
		}
		desc := "not connected"
		if h.Connected {
			desc = "connected"
		}
		items = append(items, menuItem{word: h.Name, desc: desc})
	}
	return items, nil
}

func (m *shellModel) helpText() string {
	var b strings.Builder
	b.WriteString(stBold.Render("vp shell commands") +
		stDim.Render(" — a leading space sends the line to the shell instead") + "\n")
	for _, c := range slashCmds {
		fmt.Fprintf(&b, "  %-18s %s\n", strings.TrimSpace("/"+c.name+" "+c.args),
			stDim.Render(c.help))
	}
	b.WriteString("\n" + stBold.Render("keys") + "\n")
	keys := [][2]string{
		{"tab", "complete, with the shell on this session's machine"},
		{"↑ ↓", "history; the menu, when one is open"},
		{"alt+⏎", "a newline in the line"},
		{"pgup, wheel", "scroll back (in tmux: its copy mode; q returns)"},
		{"ctrl+c", "clear the line, or interrupt the running command"},
		{"ctrl+d", "exit the shell"},
		{"ctrl+\\", "detach, leaving it running"},
	}
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-18s %s\n", k[0], stDim.Render(k[1]))
	}
	return b.String()
}

// exitShell asks the shell to end, as ctrl+d on an empty line does.
func (m *shellModel) exitShell() tea.Cmd {
	m.send([]byte("exit\r"))
	m.ph = phSent
	m.sentSeq++
	return nil
}

// controlMsg is the answer to a request made on a connection of its own, so a
// slow one — moving to a machine whose pod is being built — never holds up the
// session's.
type controlMsg struct {
	text string
	err  error
}

func (m *shellModel) controlCmd(req *proto.Msg, render func(*proto.Msg) string) tea.Cmd {
	return func() tea.Msg {
		r, err := m.control(req)
		if err != nil {
			return controlMsg{err: err}
		}
		return controlMsg{text: render(r)}
	}
}

func (m *shellModel) control(req *proto.Msg) (*proto.Msg, error) {
	if m.cfg.Dial == nil {
		return nil, fmt.Errorf("no connection to the daemon")
	}
	c, err := m.cfg.Dial()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	req.Pod = m.cfg.Pod
	if err := c.Send(req); err != nil {
		return nil, err
	}
	for {
		r, _, err := c.Recv()
		if err != nil {
			return nil, err
		}
		switch r.Op {
		case proto.OpProgress:
			continue
		case proto.OpErr:
			if strings.HasPrefix(r.Err, "unknown op") {
				return nil, fmt.Errorf("the running daemon is older than this vp and " +
					"cannot do that; `vibepod down` your pods and restart it")
			}
			return nil, fmt.Errorf("%s", r.Err)
		}
		return r, nil
	}
}

// completeMsg is what the shell on the session's machine offered.
type completeMsg struct {
	seq   int
	value string
	from  int
	items []menuItem
	err   error
}

// startComplete is Tab: ask the machine's shell about the text before the
// cursor, or complete a /command's name or argument here.
func (m *shellModel) startComplete() tea.Cmd {
	row, col := m.in.Line(), m.in.Column()
	lines := strings.Split(m.in.Value(), "\n")
	if row >= len(lines) {
		return nil
	}
	line := []rune(lines[row])
	col = min(col, len(line))
	before := string(line[:col])
	from := wordStart(line[:col])
	m.compSeq++
	seq, value := m.compSeq, m.in.Value()

	if c, _, ok := slashLine(m.in.Value()); ok && row == 0 && strings.Contains(before, " ") {
		if c.values == nil {
			return nil
		}
		return func() tea.Msg {
			items, err := c.values(m)
			word := string(line[from:col])
			var keep []menuItem
			for _, it := range items {
				if strings.HasPrefix(it.word, word) {
					keep = append(keep, it)
				}
			}
			return completeMsg{seq: seq, value: value, from: from, items: keep, err: err}
		}
	}

	m.busy = "completing…"
	fpath := m.fpath[m.backend]
	return tea.Batch(m.tick(), func() tea.Msg {
		req := &proto.Msg{Op: proto.OpComplete, Session: m.cfg.Session, Cwd: m.cwd,
			Data: []byte(before)}
		if fpath != "" {
			req.Env = []string{"VP_FPATH=" + fpath}
		}
		r, err := m.control(req)
		if err != nil {
			return completeMsg{seq: seq, value: value, from: from, err: err}
		}
		var items []menuItem
		for _, c := range complete.Parse(r.Detail) {
			items = append(items, menuItem{word: shellWord(c.Word), label: c.Word, desc: c.Desc})
		}
		return completeMsg{seq: seq, value: value, from: from, items: items}
	})
}

// completed puts what came back into the line: the one candidate, or as much
// as they all share and a menu of the rest.
func (m *shellModel) completed(msg completeMsg) {
	m.busy = ""
	if msg.seq != m.compSeq || msg.value != m.in.Value() || m.ph != phIdle {
		return // typed on, or moved on, while the machine was asked
	}
	if msg.err != nil {
		m.flash = msg.err.Error()
		return
	}
	switch len(msg.items) {
	case 0:
		m.flash = "no completions"
		return
	case 1:
		m.acceptCompletion(msg.items[0].word, msg.from)
		return
	}
	words := make([]string, len(msg.items))
	for i, it := range msg.items {
		words[i] = it.word
	}
	cur := m.wordAtCursor(msg.from)
	if p := commonPrefix(words); len(p) > len(cur) {
		m.replaceWord(msg.from, p)
	}
	m.menu = &menu{items: msg.items, sel: -1, from: msg.from}
}

func (m *shellModel) acceptCompletion(word string, from int) {
	if !strings.HasSuffix(word, "/") && !strings.HasSuffix(word, "=") &&
		!strings.HasSuffix(word, ":") {
		word += " "
	}
	m.replaceWord(from, word)
	m.menu = nil
}

func (m *shellModel) wordAtCursor(from int) string {
	lines := strings.Split(m.in.Value(), "\n")
	row := m.in.Line()
	if row >= len(lines) {
		return ""
	}
	line := []rune(lines[row])
	col := min(m.in.Column(), len(line))
	if from > col {
		return ""
	}
	return string(line[from:col])
}

// replaceWord replaces the current line's runes from `from` to the cursor.
func (m *shellModel) replaceWord(from int, word string) {
	lines := strings.Split(m.in.Value(), "\n")
	row := m.in.Line()
	if row >= len(lines) {
		return
	}
	line := []rune(lines[row])
	col := min(m.in.Column(), len(line))
	from = min(from, col)
	lines[row] = string(line[:from]) + word + string(line[col:])
	m.in.SetValue(strings.Join(lines, "\n"))
	// SetValue leaves the cursor at the end; put it after the word.
	m.in.MoveToBegin()
	for i := 0; m.in.Line() < row && i < 1000; i++ {
		m.in.CursorDown()
	}
	m.in.SetCursorColumn(from + len([]rune(word)))
}

// wordStart is where the word ending at the cursor begins: after the last
// space that is not escaped.
func wordStart(r []rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == ' ' || r[i] == '\t' {
			if i > 0 && r[i-1] == '\\' {
				continue
			}
			return i + 1
		}
	}
	return 0
}

// shellWord makes a candidate safe to put in a command line. zsh hands its
// candidates back quoted; bash's compgen does not, so a name with a space in it
// is escaped here.
func shellWord(w string) string {
	if !strings.Contains(w, " ") || strings.Contains(w, `\ `) {
		return w
	}
	var b strings.Builder
	for _, r := range w {
		if strings.ContainsRune(" '\"$`\\!&;()<>|*?[]#~", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func commonPrefix(words []string) string {
	if len(words) == 0 {
		return ""
	}
	sorted := append([]string(nil), words...)
	sort.Strings(sorted)
	a, z := []rune(sorted[0]), []rune(sorted[len(sorted)-1])
	i := 0
	for i < len(a) && i < len(z) && a[i] == z[i] {
		i++
	}
	return string(a[:i])
}

// menuView draws the open menu, a window of it around the selection.
func (m *shellModel) menuView() []string {
	mu := m.menu
	if mu == nil || len(mu.items) == 0 {
		return nil
	}
	top := 0
	if mu.sel >= menuRows {
		top = mu.sel - menuRows + 1
	}
	end := min(top+menuRows, len(mu.items))
	labelW := 0
	for _, it := range mu.items[top:end] {
		labelW = max(labelW, lipgloss.Width(it.labelText()))
	}
	labelW = min(labelW, max(m.w/2, 10))
	var out []string
	for i := top; i < end; i++ {
		it := mu.items[i]
		label := lipgloss.NewStyle().Width(labelW).MaxWidth(labelW).Render(it.labelText())
		row := "  " + label
		if i == mu.sel {
			row = stAccent.Render("› ") + stAccent.Bold(true).Render(label)
		}
		if it.desc != "" {
			row += "  " + stDim.Render(it.desc)
		}
		out = append(out, lipgloss.NewStyle().MaxWidth(m.w-1).Render(" "+row))
	}
	if more := len(mu.items) - end; more > 0 {
		out = append(out, stDim.Render(fmt.Sprintf("   … %d more", more)))
	}
	return out
}

func (it menuItem) labelText() string {
	if it.label != "" {
		return it.label
	}
	return it.word
}

// Scrolling back.
//
// The blocks are in the terminal's own scrollback, so scrolling is the
// terminal's. Inside tmux that means copy mode, which normally takes a prefix
// key to reach; here PgUp and the wheel enter it directly, and scrolling back to
// the bottom leaves it (copy-mode -e). The wheel reaches this program only
// because it asks for mouse events, which it does only inside tmux: anywhere
// else the terminal scrolls on its own, and selecting text needs no shift.

func tmuxPane() string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	return os.Getenv("TMUX_PANE")
}

// tmuxQuietClear stops tmux copying the screen into the scrollback whenever the
// whole of it is erased (scroll-on-clear), for this pane and while vp shell
// runs: Bubble Tea erases the whole screen when the window is resized, and
// what is on the screen here is managed, not lost — it would arrive in the
// scrollback twice. The returned function puts back what the pane had.
func tmuxQuietClear(pane string) func() {
	if pane == "" {
		return func() {}
	}
	prev, err := exec.Command("tmux", "show", "-pqv", "-t", pane, "scroll-on-clear").Output()
	if err != nil {
		return func() {}
	}
	if exec.Command("tmux", "set", "-pq", "-t", pane, "scroll-on-clear", "off").Run() != nil {
		return func() {}
	}
	return func() {
		if v := strings.TrimSpace(string(prev)); v != "" {
			_ = exec.Command("tmux", "set", "-pq", "-t", pane, "scroll-on-clear", v).Run()
		} else {
			_ = exec.Command("tmux", "set", "-pqu", "-t", pane, "scroll-on-clear").Run()
		}
	}
}

// tmuxScroll puts the pane in copy mode and scrolls it up, a page or n lines.
func (m *shellModel) tmuxScroll(page bool, n int) tea.Cmd {
	pane := m.tmux
	if pane == "" {
		return nil
	}
	return func() tea.Msg {
		args := []string{"copy-mode", "-e", "-t", pane}
		if page {
			args = []string{"copy-mode", "-e", "-u", "-t", pane}
		} else {
			args = append(args, ";", "send-keys", "-X", "-N", fmt.Sprint(n),
				"-t", pane, "scroll-up")
		}
		if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
			return controlMsg{err: fmt.Errorf("tmux: %s", strings.TrimSpace(string(out)))}
		}
		return nil
	}
}

// Clearing.
//
// `clear` in a block clears the block's own emulator, which is not what anyone
// means by it: its output has already gone to the real scrollback. So when a
// command asks for the scrollback to go — ESC [ 3 J, which clear, tput clear
// and reset all send — the real terminal is cleared too, and /clear does the
// same without a shell.

const clearSeq = "\x1b[H\x1b[2J\x1b[3J"
