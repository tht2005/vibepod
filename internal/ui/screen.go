package ui

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"vibepod/internal/term"
)

// scrollAway starts the interface on a clean screen without losing what was on
// it: the lines above the cursor are scrolled up into the scrollback — by
// newlines at the bottom, which every terminal and tmux keep, where an erase
// would discard them — and the cursor goes to the top.
func scrollAway(rows int) {
	if rows <= 0 || !term.IsTTY(os.Stdout) {
		return
	}
	n := rows
	if row := cursorRow(); row > 0 {
		n = row // only what is there, not a screenful of blank lines
	}
	// The interface's frame starts on the second row; see frame.go.
	fmt.Printf("\x1b[%d;1H%s\x1b[H\x1b[2J\x1b[2;1H", rows, strings.Repeat("\n", n))
}

// cursorRow asks the terminal where the cursor is, and returns its row counted
// from 1, or 0 when the terminal does not say in time.
func cursorRow() int {
	fd := os.Stdin.Fd()
	st, err := term.MakeRaw(fd)
	if err != nil {
		return 0
	}
	defer st.Restore()
	if _, err := os.Stdout.WriteString("\x1b[6n"); err != nil {
		return 0
	}
	// The answer is ESC [ row ; col R.
	var got []byte
	buf := make([]byte, 32)
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if n, err := unix.Poll(pfd, int(time.Until(deadline).Milliseconds())+1); err != nil || n == 0 {
			break
		}
		n, err := unix.Read(int(fd), buf)
		if err != nil || n <= 0 {
			break
		}
		got = append(got, buf[:n]...)
		if i := strings.LastIndexByte(string(got), 'R'); i >= 0 {
			s := string(got[:i])
			if j := strings.LastIndex(s, "\x1b["); j >= 0 {
				row, _, _ := strings.Cut(s[j+2:], ";")
				r, _ := strconv.Atoi(row)
				return r
			}
		}
	}
	return 0
}
