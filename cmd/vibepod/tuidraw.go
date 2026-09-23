package main

import (
	"fmt"
	"os"
	"strings"

	"vibepod/internal/proto"
)

// Drawing the cockpit.
//
// One frame is built as a string and written once. Writing a screen in pieces
// shows as tearing on a slow pty, and a cockpit that flickers while an agent
// works is a cockpit people close.

const (
	dim   = "\x1b[2m"
	bold  = "\x1b[1m"
	reset = "\x1b[0m"
	rev   = "\x1b[7m"
)

func (co *cockpit) draw() {
	co.mu.Lock()
	defer co.mu.Unlock()
	if co.handoff != nil || co.quit {
		return // the session owns the screen
	}
	rows, cols := co.rows, co.cols
	if rows < 8 || cols < 40 {
		// Too small to divide. Say so rather than drawing something broken.
		os.Stdout.WriteString("\x1b[H\x1b[2Jvibepod: the cockpit needs at least 40x8\r\n")
		return
	}
	left := cols / 2
	if left > 40 {
		left = 40
	}
	right := cols - left - 1

	var b strings.Builder
	b.WriteString("\x1b[H")
	title := fmt.Sprintf("vibepod · %s", co.pod)
	b.WriteString(bold + pad(title, cols) + reset + "\r\n")

	lines := make([]string, 0, rows)
	lines = append(lines, co.leftRight(co.machineLines(left), co.activityLines(right),
		left, right)...)

	body := rows - 3 // title, status, keys
	for i := 0; i < body; i++ {
		if i < len(lines) {
			b.WriteString("\x1b[K" + lines[i] + "\r\n")
		} else {
			b.WriteString("\x1b[K\r\n")
		}
	}
	// Status, then the keymap. The backend of the focused session is on the
	// status line because it is the thing you most need to know before pressing
	// a key.
	status := co.status
	if co.prompt != nil {
		status = co.prompt.label + string(co.prompt.line) + "▏"
	}
	b.WriteString("\x1b[K" + dim + pad(trim(status, cols), cols) + reset + "\r\n")
	b.WriteString("\x1b[K" + rev + pad(trim(co.keyline(), cols), cols) + reset)
	b.WriteString("\x1b[J")
	os.Stdout.WriteString(b.String())
}

func (co *cockpit) keyline() string {
	backend := "—"
	if co.selSess < len(co.sessions) {
		backend = co.sessions[co.selSess].Backend
	}
	return fmt.Sprintf(" backend %s │ m mount  u unmount  b backend  ⏎ attach  "+
		"⇥ pane  / filter  q quit", backend)
}

// machineLines is the left column: the machines, then the sessions. Both are
// lists of things you can act on, so they share a column and the focus moves
// between them.
func (co *cockpit) machineLines(w int) []string {
	out := []string{bold + "MACHINES" + reset}
	for i, h := range co.hosts {
		dot := "●"
		switch {
		case h.Local:
			dot = "●"
		case !h.Mounted:
			dot = "○"
		case !h.Connected:
			dot = "◌"
		}
		where := "local"
		switch {
		case !h.Mounted:
			where = "not mounted"
		case len(h.Dirs) > 0:
			where = short(h.Dirs[0])
		}
		row := fmt.Sprintf("%s %-9s %s", dot, h.Name, where)
		if h.Default {
			row = fmt.Sprintf("%s %-9s %s", dot, h.Name+"*", where)
		}
		out = append(out, co.mark(row, w, co.focus == 0 && i == co.selHost))
	}
	out = append(out, "", bold+"SESSIONS"+reset)
	if len(co.sessions) == 0 {
		out = append(out, dim+"  none — `vp shell` opens one"+reset)
	}
	for i, s := range co.sessions {
		kind := s.Kind
		if kind == "" {
			kind = "session"
		}
		row := fmt.Sprintf("%-3s %-10s ▸ %s", s.ID, kind, s.Backend)
		out = append(out, co.mark(row, w, co.focus == 1 && i == co.selSess))
	}
	out = append(out, "", bold+"MOUNTS"+reset)
	for _, m := range co.mounts {
		out = append(out, trim(fmt.Sprintf("  %-16s %s", short(m.At),
			shortSource(m)), w))
	}
	return out
}

// mark highlights the focused row. The marker is a reverse-video line rather
// than a colour, so it survives a terminal with an unhelpful palette.
func (co *cockpit) mark(row string, w int, sel bool) string {
	row = trim(row, w)
	if !sel {
		return row
	}
	return rev + pad(row, w) + reset
}

func shortSource(m proto.TreeMount) string {
	if m.Owner == "pod" {
		return "local"
	}
	return m.Owner + ":" + trim(m.Source[strings.Index(m.Source, ":")+1:], 22)
}

// activityLines is the right column: the live log, newest at the bottom, which is
// where a terminal reader's eye already is.
func (co *cockpit) activityLines(w int) []string {
	out := []string{bold + "ACTIVITY" + reset}
	lines := co.activity
	if co.filter != "" {
		kept := make([]string, 0, len(lines))
		for _, l := range lines {
			if strings.Contains(l, co.filter) {
				kept = append(kept, l)
			}
		}
		lines = kept
		out[0] = bold + "ACTIVITY" + reset + dim + "  /" + co.filter + reset
	}
	// Only as much as fits, and the tail is what matters.
	room := co.rows - 5
	if room < 1 {
		room = 1
	}
	if len(lines) > room {
		lines = lines[len(lines)-room:]
	}
	for _, l := range lines {
		out = append(out, trim(l, w))
	}
	if len(lines) == 0 {
		out = append(out, dim+"  waiting for the first command"+reset)
	}
	return out
}

// leftRight puts two columns side by side, with a divider that does not depend on
// either column's length.
func (co *cockpit) leftRight(l, r []string, lw, rw int) []string {
	n := len(l)
	if len(r) > n {
		n = len(r)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		a, b := "", ""
		if i < len(l) {
			a = l[i]
		}
		if i < len(r) {
			b = r[i]
		}
		out = append(out, pad(a, lw)+dim+"│"+reset+trim(b, rw))
	}
	return out
}

// pad and trim count printable width, so an escape sequence does not eat a
// column. Nothing here uses wide characters in a position where one column of
// slop would matter.
func pad(s string, w int) string {
	n := visibleLen(s)
	if n >= w {
		return trim(s, w)
	}
	return s + strings.Repeat(" ", w-n)
}

func visibleLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc && (r == 'm' || r == 'K' || r == 'J' || r == 'H'):
			esc = false
		case esc:
		case r == 0x1b:
			esc = true
		default:
			n++
		}
	}
	return n
}
