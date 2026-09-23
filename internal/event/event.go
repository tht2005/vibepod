// Package event is the daemon's single source of truth about what ran where.
//
// One stream, several renderings: the console pane, `vpctl tree -f` and
// `vpctl log -f` are all subscribers. Collection is solved by the exec gate,
// so this is only a fan-out problem.
package event

import (
	"sync"
	"time"
)

// Kinds of event. The set is small on purpose: an agent parsing this stream
// is an API consumer, and it will outlive several rounds of visual layout.
const (
	KindExec    = "exec"
	KindExit    = "exit"
	KindMount   = "mount"
	KindPod     = "pod"
	KindSession = "session"
	// KindNotice is something vibepod did that the caller did not ask for and
	// would otherwise not learn about — a refused environment variable, say.
	// It exists so that such things are never merely silent.
	KindNotice = "notice"
)

// Version is the `v` field. It changes only when the shape does.
const Version = 1

// Event is one line of the NDJSON stream.
type Event struct {
	V      int      `json:"v"`
	Kind   string   `json:"ev"`
	Time   string   `json:"ts"`
	Pod    string   `json:"pod,omitempty"`
	PID    int      `json:"pid,omitempty"`
	PPID   int      `json:"ppid,omitempty"`
	Argv   []string `json:"argv,omitempty"`
	Cwd    string   `json:"cwd,omitempty"`
	Target string   `json:"target,omitempty"`

	// Exit only. Code is a pointer because vibepod is not the parent of every
	// process it observes: for a pod-local exec it knows the command ended but
	// not what it returned, and saying 0 there would be a lie.
	Code      *int  `json:"code,omitempty"`
	ElapsedMS int64 `json:"elapsed_ms,omitempty"`

	Session string `json:"session,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Bus fans events out to subscribers. A slow subscriber is dropped rather
// than allowed to stall the exec gate, which every process in a pod waits on.
type Bus struct {
	mu     sync.Mutex
	next   int
	subs   map[int]chan Event
	recent []Event
	max    int
}

func NewBus(history int) *Bus {
	return &Bus{subs: map[int]chan Event{}, max: history}
}

func (b *Bus) Publish(e Event) {
	e.V = Version
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339Nano)
	}
	b.mu.Lock()
	b.recent = append(b.recent, e)
	if len(b.recent) > b.max {
		b.recent = b.recent[len(b.recent)-b.max:]
	}
	subs := make([]chan Event, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default: // a subscriber that cannot keep up does not get to block a pod
		}
	}
}

// Subscribe returns a channel of future events and a function to stop.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, id)
		close(ch)
		b.mu.Unlock()
	}
}

// History is what happened before the subscriber arrived.
func (b *Bus) History() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Event(nil), b.recent...)
}
