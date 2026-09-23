package daemon

import (
	"fmt"
	"time"

	"vibepod/internal/proto"
)

// Progress reports what a slow request is doing while it does it.
//
// Creating a pod can mean waiting on several machines, each with a ten-second
// budget before it is declared unreachable. Without this the whole wait is
// indistinguishable from a hang, and the error at the end arrives with no
// indication of which host it was about or how long it took.
type Progress struct {
	send func(*proto.Msg)
	id   uint64
	at   time.Time
}

// step announces work about to start, leaving the line open for its outcome.
func (p *Progress) step(format string, a ...any) {
	if p == nil || p.send == nil {
		return
	}
	p.at = time.Now()
	p.send(&proto.Msg{Op: proto.OpProgress, ID: p.id, Partial: true,
		Detail: fmt.Sprintf(format, a...)})
}

// ok closes the line opened by step, with how long it took: a host that works
// but is slow is worth seeing as clearly as one that fails.
func (p *Progress) ok(format string, a ...any) {
	if p == nil || p.send == nil {
		return
	}
	detail := fmt.Sprintf(format, a...)
	if !p.at.IsZero() {
		detail = fmt.Sprintf("%s (%s)", detail, took(time.Since(p.at)))
	}
	p.send(&proto.Msg{Op: proto.OpProgress, ID: p.id, Detail: detail})
}

// failed closes the line without the reason, because the request is about to
// return that reason as its error. The progress line says which step stopped;
// the error says why. Printing both would say it twice.
func (p *Progress) failed() {
	if p == nil || p.send == nil {
		return
	}
	p.send(&proto.Msg{Op: proto.OpProgress, ID: p.id, Detail: "failed"})
}

func took(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
