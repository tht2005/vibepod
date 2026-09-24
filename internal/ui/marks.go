// Package ui is vibepod's terminal interface: the `vp shell` frontend and the
// cockpit, both drawn with Bubble Tea.
package ui

import (
	"bytes"
	"strconv"
	"strings"
)

// A shell's output, cut at the places a person thinks of as boundaries.
//
// The hooks `vp shell` installs make the shell say where its prompt starts and
// ends, where a command's output starts, and how it ended — FinalTerm's OSC 133,
// which is what Warp, iTerm2 and kitty read too. The daemon adds its own OSC,
// 7717, for what only it knows: which machine's shell this now is, and where
// replayed scrollback stops. Everything else passes through untouched, escape
// sequences included, because the running program's output is the running
// program's business.

// Kind is what one piece of the stream is.
type Kind int

const (
	Text     Kind = iota // bytes for whoever is showing them
	Prompt               // 133;A — the shell is about to draw its prompt
	Input                // 133;B — the prompt is drawn; what follows is the line being typed
	Exec                 // 133;C — the command line was accepted; its output follows
	Done                 // 133;D — the command finished, with Code
	Cwd                  // 7717;home=…;cwd=… — where the shell now is
	Backend              // 7717;backend=… — the session now shows this machine's shell
	Replayed             // 7717;replayed — the replayed scrollback ends here
	Fpath                // 7717;fpath=… — where zsh finds its completions
)

// Event is one piece of the stream.
type Event struct {
	Kind Kind
	Data []byte // Text
	Code int    // Done
	// Cwd and Home are what the shell reported; Backend and Ended are the
	// machine now shown and, when a shell went away, the one that did.
	Cwd, Home      string
	Backend, Ended string
	// Fpath is the zsh function path, so Tab completes with the same
	// completion functions the shell itself would use.
	Fpath string
}

// maxOSC bounds how long an unterminated OSC may grow before it is judged not
// to be one of ours and given back as text: a program printing a stray ESC ]
// must not make everything after it vanish. It is still generous, because a
// zsh fpath with many plugins is a long marker.
const maxOSC = 64 << 10

// Parser splits a byte stream into events, carrying a partial escape sequence
// across reads.
type Parser struct {
	pend []byte
}

// Feed consumes b and returns the events it completes. Text events are
// coalesced, and a Text event's Data does not alias b.
func (p *Parser) Feed(b []byte) []Event {
	data := b
	if len(p.pend) > 0 {
		data = append(p.pend, b...)
		p.pend = nil
	}
	var out []Event
	var text []byte
	flush := func() {
		if len(text) > 0 {
			out = append(out, Event{Kind: Text, Data: text})
			text = nil
		}
	}
	for len(data) > 0 {
		i := bytes.IndexByte(data, 0x1b)
		if i < 0 {
			text = append(text, data...)
			break
		}
		text = append(text, data[:i]...)
		data = data[i:]
		if len(data) < 2 {
			p.pend = append([]byte(nil), data...)
			break
		}
		if data[1] != ']' {
			text = append(text, data[:2]...)
			data = data[2:]
			continue
		}
		body, n := oscEnd(data[2:])
		if n < 0 {
			if len(data) > maxOSC {
				text = append(text, data[:2]...)
				data = data[2:]
				continue
			}
			p.pend = append([]byte(nil), data...)
			break
		}
		seq := data[:2+n]
		data = data[2+n:]
		if ev, ok := parseOSC(body); ok {
			flush()
			out = append(out, ev)
			continue
		}
		text = append(text, seq...)
	}
	flush()
	return out
}

// oscEnd finds an OSC's terminator, BEL or ST, returning its body and the length
// consumed, or n < 0 if it has not arrived yet.
func oscEnd(b []byte) (body string, n int) {
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case 0x07:
			return string(b[:i]), i + 1
		case 0x1b:
			if i+1 >= len(b) {
				return "", -1
			}
			if b[i+1] == '\\' {
				return string(b[:i]), i + 2
			}
		}
	}
	return "", -1
}

func parseOSC(body string) (Event, bool) {
	switch {
	case strings.HasPrefix(body, "133;"):
		f := strings.Split(body[4:], ";")
		switch f[0] {
		case "A":
			return Event{Kind: Prompt}, true
		case "B":
			return Event{Kind: Input}, true
		case "C":
			return Event{Kind: Exec}, true
		case "D":
			ev := Event{Kind: Done}
			if len(f) > 1 {
				ev.Code, _ = strconv.Atoi(f[1])
			}
			return ev, true
		}
	case strings.HasPrefix(body, "7717;"):
		kv := body[5:]
		switch {
		case kv == "replayed":
			return Event{Kind: Replayed}, true
		case strings.HasPrefix(kv, "backend="):
			ev := Event{Kind: Backend}
			for _, f := range strings.Split(kv, ";") {
				k, v, _ := strings.Cut(f, "=")
				switch k {
				case "backend":
					ev.Backend = v
				case "ended":
					ev.Ended = v
				}
			}
			return ev, true
		case strings.HasPrefix(kv, "fpath="):
			return Event{Kind: Fpath, Fpath: kv[len("fpath="):]}, true
		case strings.HasPrefix(kv, "home="):
			// cwd last and taken whole: a directory name may contain anything.
			ev := Event{Kind: Cwd}
			rest := kv[len("home="):]
			if i := strings.Index(rest, ";cwd="); i >= 0 {
				ev.Home, ev.Cwd = rest[:i], rest[i+len(";cwd="):]
			} else {
				ev.Home = rest
			}
			return ev, true
		}
	}
	return Event{}, false
}
