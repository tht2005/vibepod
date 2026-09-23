// Package term handles terminals: the scrollback a detached session keeps,
// and putting a client's own terminal into raw mode.
package term

import "sync"

// Ring is a fixed-size scrollback buffer.
//
// It is what makes detaching useful: the daemon owns the PTY and keeps
// writing into this whether anyone is watching or not, so reattaching can
// replay what was missed. dtach-style, built in — no tmux dependency and no
// prefix key to collide with the agent's own.
type Ring struct {
	mu   sync.Mutex
	buf  []byte
	size int
	full bool
	pos  int
}

func NewRing(size int) *Ring {
	return &Ring{buf: make([]byte, size), size: size}
}

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n >= r.size {
		copy(r.buf, p[n-r.size:])
		r.pos = 0
		r.full = true
		return n, nil
	}
	for _, b := range p {
		r.buf[r.pos] = b
		r.pos++
		if r.pos == r.size {
			r.pos = 0
			r.full = true
		}
	}
	return n, nil
}

// Snapshot returns the scrollback in order.
func (r *Ring) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]byte(nil), r.buf[:r.pos]...)
	}
	out := make([]byte, 0, r.size)
	out = append(out, r.buf[r.pos:]...)
	return append(out, r.buf[:r.pos]...)
}
