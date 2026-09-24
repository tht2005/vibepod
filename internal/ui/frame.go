package ui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"vibepod/internal/sys"
)

// The screen, top to bottom: the latest output, then the running block, then
// the input box on the terminal's bottom row — always there, however little
// has been printed.
//
// The interface is one frame from the second row to the bottom. What has been
// printed fills the screen from just above the box upwards; a line that no
// longer fits leaves through the top into the terminal's own scrollback, the
// way output does in a plain terminal. That is done by scrolling only the rows
// above the box — a scroll region whose top is the first row, which terminals
// and tmux keep in their history — so the box never moves and the scrollback
// gets each line once, in order, with its colours.
//
// The first row is output too, but painted here rather than by Bubble Tea.
// Bubble Tea redraws a frame by erasing from its top to the end of the screen,
// and an erase that covers the whole screen is one tmux (scroll-on-clear)
// copies into the scrollback first: the box and all.

// print puts output above the box.
func (m *shellModel) print(s string) {
	if m.quiet {
		return
	}
	s = strings.TrimSuffix(s, "\n")
	w := max(m.w, 1)
	for _, l := range strings.Split(s, "\n") {
		// One row each, so the rows above the box can be counted: a line wider
		// than the terminal is wrapped here rather than by the terminal.
		m.tail = append(m.tail, strings.Split(ansi.Hardwrap(l, w, true), "\n")...)
	}
}

// tailRows is how many rows are left above the box and the running block.
func (m *shellModel) tailRows() int {
	chrome, _ := m.chrome()
	return m.h - len(chrome)
}

// fit moves what no longer fits above the box into the scrollback, and says
// what has to be written for that to happen.
func (m *shellModel) fit() tea.Cmd {
	if m.w == 0 || m.h == 0 || m.ph == phHand || m.ph == phRaw || m.quitting || m.realigning {
		return nil
	}
	if r := m.tailRows(); r >= 1 && len(m.tail) > r {
		k := len(m.tail) - r
		m.pending = append(m.pending, m.tail[:k]...)
		m.tail = append([]string(nil), m.tail[k:]...)
	}
	if len(m.pending) == 0 && !m.clearing && m.firstRow() == m.row1 {
		return nil
	}
	var cmds []tea.Cmd
	cmds = append(cmds, tea.Raw(frameWrite{m}))
	if m.clearing {
		cmds = append(cmds, tea.ClearScreen)
	}
	return tea.Sequence(cmds...)
}

// firstRow is what the screen's first row should show.
func (m *shellModel) firstRow() string {
	if tv := m.tailView(m.tailRows()); len(tv) > 0 {
		return tv[0]
	}
	return ""
}

// clearTerminal empties the screen and the scrollback: what `clear` means.
func (m *shellModel) clearTerminal() {
	if m.quiet {
		return
	}
	m.tail, m.pending, m.clearing = nil, nil, true
}

// frameWrite is what goes to the terminal outside the frame Bubble Tea draws:
// a clear, and lines leaving through the top. It is rendered when Bubble Tea
// writes it rather than when it is queued, on the goroutine that runs Update,
// so it always writes the model as it is — several queued writes cannot land
// out of order, and one queued before a clear finds nothing left to push.
type frameWrite struct{ m *shellModel }

func (f frameWrite) String() string {
	m := f.m
	var b strings.Builder
	if m.clearing {
		m.clearing = false
		// Saved and restored around it: Bubble Tea moves the cursor relative to
		// where it last left it.
		b.WriteString("\x1b7" + clearSeq + "\x1b8")
		m.histBase, m.pushed, m.recent = 0, 0, nil
	}
	// A scroll region is two rows at least; with less room than that — a menu
	// open on a short terminal — the lines wait, in order, for the room.
	r := m.tailRows()
	if len(m.pending) == 0 || r < 2 {
		if r >= 1 {
			m.row1 = m.firstRow()
			fmt.Fprintf(&b, "\x1b7\x1b[1;1H%s\x1b[m\x1b[K\x1b8", m.row1)
		}
		return b.String()
	}
	b.WriteString("\x1b7")
	fmt.Fprintf(&b, "\x1b[1;%dr", r)
	for _, l := range m.pending {
		// Onto the top row, then scroll the region by one: the top row goes to
		// the scrollback, as this line.
		fmt.Fprintf(&b, "\x1b[1;1H%s\x1b[m\x1b[K\x1b[%d;1H\n", l, r)
	}
	m.pushed += len(m.pending)
	m.remember(m.pending)
	m.pending = nil
	b.WriteString("\x1b[r")
	// The region now holds what scrolled up, which is not what Bubble Tea
	// thinks is there; put back what is.
	for i, l := range m.tailView(r) {
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[m\x1b[K", i+1, l)
	}
	m.row1 = m.firstRow()
	b.WriteString("\x1b8")
	return b.String()
}

// tailView is the rows above the box: blank ones first while there is room,
// then the latest output.
func (m *shellModel) tailView(r int) []string {
	if r <= 0 {
		return nil
	}
	t := m.tail
	if len(t) > r {
		t = t[len(t)-r:]
	}
	out := make([]string, 0, r)
	for i := len(t); i < r; i++ {
		out = append(out, "")
	}
	return append(out, t...)
}

// Resizing.
//
// A terminal moves lines between the screen and its scrollback when it changes
// height — tmux and most others push the top rows up when it shrinks and pull
// them back when it grows — and moves the cursor with them, which Bubble Tea
// does not know. A frame the height of the screen drawn from where Bubble Tea
// now thinks it starts would scroll the terminal and push rows of it into the
// scrollback. So until the size settles the frame is one line, which cannot
// scroll anything; then Bubble Tea steps aside as it does for a full-screen
// program, every row is cleared (row by row: a whole-screen erase is one tmux
// may copy into the scrollback), the cursor goes where that one line starts —
// the second row — and the frame is redrawn from there.

type (
	realignMsg   struct{ seq int }
	realignedMsg struct{}
)

// resized accounts for a change of size, and asks for the frame to be put
// back once it has settled.
func (m *shellModel) resized(w, h int) tea.Cmd {
	// How many rows the terminal moved into its scrollback (or, negative, back
	// out of it). tmux says exactly; anything else is taken to push the rows a
	// shrink cut off the top, and to pull nothing back.
	moved := max(m.h-h, 0)
	if n, ok := tmuxHistory(m.tmux); ok {
		// A full scrollback drops a line for each one added, and then its size
		// says nothing.
		if n < m.histLimit && m.histBase+m.pushed < m.histLimit {
			moved = n - (m.histBase + m.pushed)
		}
		m.histBase, m.pushed = n, 0
	}
	if moved > 0 {
		// Those rows came off the top of the screen, blank ones included: what
		// was output there is in the scrollback now, and must not be pushed again.
		tv := m.tailView(m.tailRows())
		pad := len(tv) - len(m.tail)
		m.remember(tv[:min(moved, len(tv))])
		if k := min(max(moved-pad, 0), len(m.tail)); k > 0 {
			m.tail = append([]string(nil), m.tail[k:]...)
		}
	} else if moved < 0 {
		// Pulled back onto the screen, which is about to be cleared: they go back
		// where they were.
		n := min(-moved, len(m.recent))
		back := m.recent[len(m.recent)-n:]
		m.recent = m.recent[:len(m.recent)-n]
		m.pending = append(append([]string(nil), back...), m.pending...)
	}
	for i, l := range m.tail {
		m.tail[i] = ansi.Truncate(l, w, "")
	}
	m.realigning = true
	m.realignSeq++
	seq := m.realignSeq
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg { return realignMsg{seq} })
}

// realign is the step aside: clear every row and leave the cursor on the last.
type realign struct{}

func (realign) SetStdin(io.Reader)  {}
func (realign) SetStdout(io.Writer) {}
func (realign) SetStderr(io.Writer) {}

func (realign) Run() error {
	rows, _, err := sys.GetWinsize(os.Stdout.Fd())
	if err != nil || rows <= 0 {
		return err
	}
	var b strings.Builder
	for r := 1; r <= rows; r++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", r)
	}
	b.WriteString("\x1b[2;1H")
	_, err = os.Stdout.WriteString(b.String())
	return err
}

// tmuxHistory is how many lines are in this pane's scrollback.
func tmuxHistory(pane string) (int, bool) {
	if pane == "" {
		return 0, false
	}
	out, err := exec.Command("tmux", "display", "-p", "-t", pane, "#{history_size}").Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return n, err == nil
}

// tmuxHistoryLimit is how many lines this pane's scrollback keeps.
func tmuxHistoryLimit(pane string) int {
	out, err := exec.Command("tmux", "display", "-p", "-t", pane, "#{history_limit}").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// remember keeps the last lines that went into the scrollback, which a
// terminal growing taller may pull back out of it.
func (m *shellModel) remember(lines []string) {
	m.recent = append(m.recent, lines...)
	if n := len(m.recent) - 256; n > 0 {
		m.recent = append([]string(nil), m.recent[n:]...)
	}
}
