package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"vibepod/internal/proto"
	"vibepod/internal/sys"
	"vibepod/internal/term"
)

// handover is a stretch of time in which the terminal belongs to the program
// and not to the interface: a full-screen program, from the moment it enters
// the alternate screen until it leaves or its command ends; or, when the hooks
// never took, the whole rest of the session. Bubble Tea steps aside for it the
// way it does for $EDITOR, which is exactly what this is.
type handover struct {
	conn    *proto.Conn
	data    chan []byte
	done    chan struct{}
	once    sync.Once
	forever bool
}

func newHandover(c *proto.Conn, forever bool) *handover {
	return &handover{conn: c, data: make(chan []byte, 4096),
		done: make(chan struct{}), forever: forever}
}

func (h *handover) end() { h.once.Do(func() { close(h.done) }) }

// SetConn is set by the model before Run: the stream that noticed the program
// does not hold the connection.
func (h *handover) setConn(c *proto.Conn) {
	if h.conn == nil {
		h.conn = c
	}
}

func (h *handover) SetStdin(io.Reader)  {}
func (h *handover) SetStdout(io.Writer) {}
func (h *handover) SetStderr(io.Writer) {}

// Run is the raw terminal: output straight to it, keys straight back, and the
// full window size for the program, until the program is done with it.
func (h *handover) Run() error {
	fd := os.Stdin.Fd()
	if st, err := term.MakeRaw(fd); err == nil {
		defer st.Restore()
	}
	full := func() {
		if r, c, err := sys.GetWinsize(fd); err == nil {
			_ = h.conn.Send(&proto.Msg{Op: proto.OpWinch, Rows: r, Cols: c})
		}
	}
	full()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.keys(int(fd), stop)
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	out := bufio.NewWriterSize(os.Stdout, 64<<10)
	for {
		select {
		case b := <-h.data:
			_, _ = out.Write(b)
			// Everything already queued goes in one write, so a program redrawing
			// its screen is not shown a line at a time.
			for more := true; more; {
				select {
				case b := <-h.data:
					_, _ = out.Write(b)
				default:
					more = false
				}
			}
			_ = out.Flush()
		case <-winch:
			full()
		case <-h.done:
			for {
				select {
				case b := <-h.data:
					_, _ = out.Write(b)
				default:
					// Bubble Tea left the cursor on the bottom row when it stepped
					// aside and carries on from there, counting relative to it. A
					// program that did not save and restore it cannot be trusted to
					// have put it back.
					if r, _, err := sys.GetWinsize(fd); err == nil && !h.forever {
						_, _ = fmt.Fprintf(out, "\x1b[%d;1H", r)
					}
					return out.Flush()
				}
			}
		}
	}
}

// keys forwards the keyboard until stop, polling so that it can stop without a
// read left blocked on the terminal — which would eat the first key meant for
// the interface when it comes back.
func (h *handover) keys(fd int, stop <-chan struct{}) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-stop:
			return
		default:
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 50)
		if err != nil && err != unix.EINTR {
			return
		}
		if n <= 0 || fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		k, err := unix.Read(fd, buf)
		if k <= 0 || err != nil {
			continue
		}
		b := buf[:k]
		for i, ch := range b {
			if ch == term.DetachKey {
				if i > 0 {
					_ = h.conn.Send(&proto.Msg{Op: proto.OpInput, Data: append([]byte(nil), b[:i]...)})
				}
				_ = h.conn.Send(&proto.Msg{Op: proto.OpDetach})
				h.end()
				return
			}
		}
		_ = h.conn.Send(&proto.Msg{Op: proto.OpInput, Data: append([]byte(nil), b...)})
	}
}

// History is one file per user, shared by every pod, the way a shell's is.

const maxHistory = 5000

func loadHistory(path string) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		out = append(out, unescapeHist(sc.Text()))
	}
	if len(out) > maxHistory {
		out = out[len(out)-maxHistory:]
	}
	return out
}

func appendHistory(path, line string) {
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(escapeHist(line) + "\n")
}

// A multi-line entry is one line in the file.
func escapeHist(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		default:
			b = append(b, s[i])
		}
	}
	return string(b)
}

func unescapeHist(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == 'n' {
				b = append(b, '\n')
			} else {
				b = append(b, s[i])
			}
			continue
		}
		b = append(b, s[i])
	}
	return string(b)
}
