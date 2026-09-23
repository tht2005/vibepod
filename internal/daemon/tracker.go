package daemon

import (
	"sort"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/sys"
)

// execRec is one command vibepod was told about: a line the pod's $SHELL
// recorded, or a command `vp` dispatched. That is why vibepod can answer "what
// is running, and where" in a way pstree cannot — pstree stops at the machine
// boundary.
//
// It is no longer every execve. §3 records what that costs: a bundled binary
// that spawns another at an absolute path is invisible here.
type execRec struct {
	PID     int
	PPID    int
	Argv    []string
	Cwd     string
	Target  string
	Session string
	Start   time.Time
	End     time.Time
	Code    *int
	// ExecID names the pid file a dispatched command left on the machine it went
	// to, which is what lets `vp tree -x` find its children there.
	ExecID string
}

func (r *execRec) done() bool { return !r.End.IsZero() }

func (r *execRec) elapsedMS() int64 {
	end := r.End
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(r.Start).Milliseconds()
}

// maxExecs bounds what one pod remembers. An hour of agent work is hundreds
// of execs; the tree collapses completed ones to a count, so the tail matters
// far less than not growing without limit.
const maxExecs = 4000

// recordExec notes a new command.
//
// A pid can report several commands in turn — the pod's $SHELL records a line and
// then execs it, and a dispatching wrapper is one process per command — so a
// record on a pid that is already tracked is the *end* of what it was running.
// Treating it as an overwrite would leave the tree attributing a shell's children
// to whatever it became.
func (s *podState) recordExec(r *execRec) {
	var replaced *execRec
	s.mu.Lock()
	if old, ok := s.execs[r.PID]; ok && r.PID != 0 && !old.done() {
		old.End = time.Now()
		replaced = old
		s.retire(old)
	}
	if r.PID != 0 {
		s.execs[r.PID] = r
	}
	s.mu.Unlock()

	if replaced != nil {
		s.publishExit(replaced, "replaced")
	}
	s.d.bus.Publish(event.Event{
		Kind: event.KindExec, Pod: s.name, PID: r.PID, PPID: r.PPID,
		Argv: r.Argv, Cwd: r.Cwd, Target: r.Target, Session: r.Session,
	})
}

// retire moves a finished program into history, which is what the tree
// collapses into a count. Caller holds the lock.
func (s *podState) retire(r *execRec) {
	s.history = append(s.history, r)
	if len(s.history) > maxExecs {
		s.history = s.history[len(s.history)-maxExecs:]
	}
}

func (s *podState) publishExit(r *execRec, detail string) {
	s.d.bus.Publish(event.Event{
		Kind: event.KindExit, Pod: s.name, PID: r.PID, Argv: r.Argv,
		Target: r.Target, Code: r.Code, ElapsedMS: r.elapsedMS(),
		Session: r.Session, Detail: detail,
	})
}

// finishExec closes a record whose command vibepod was the parent of, which is
// every dispatched one. A pod-local command is only observed, so its status stays
// absent rather than invented — see watchExits.
func (s *podState) finishExec(r *execRec, code int) {
	s.mu.Lock()
	if r.End.IsZero() {
		r.End = time.Now()
	}
	c := code
	r.Code = &c
	if r.PID != 0 {
		delete(s.execs, r.PID)
	}
	s.retire(r)
	s.mu.Unlock()
	s.publishExit(r, "")
}

// watchExits notices when tracked processes end.
//
// vibepod is not the parent of most of them — it observes execs, it does not
// own them — so this polls rather than waits, and reports no exit code unless
// something that *was* the parent supplied one. An invented zero would be
// worse than an absent field.
func (s *podState) watchExits() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		select {
		case <-s.stopped:
			return
		default:
		}
		var finished []*execRec
		s.mu.Lock()
		for pid, r := range s.execs {
			if !r.done() && !sys.Alive(r.PID) {
				r.End = time.Now()
				finished = append(finished, r)
				delete(s.execs, pid)
				s.retire(r)
			}
		}
		s.mu.Unlock()
		// The log's contract is chronological. Exits noticed in the same tick
		// arrive from a map, so order them by when they started.
		sort.Slice(finished, func(i, j int) bool {
			return finished[i].Start.Before(finished[j].Start)
		})
		for _, r := range finished {
			s.publishExit(r, "")
		}
	}
}

// setExitCode records a status for a command whose parent vibepod actually was.
func (s *podState) setExitCode(pid, code int) {
	s.mu.Lock()
	if r, ok := s.execs[pid]; ok {
		c := code
		r.Code = &c
	}
	s.mu.Unlock()
}

// tree assembles the pod's structure: what is mounted from where, and what is
// executing where. The two halves belong in one view because the mount half
// explains the exec half.
func (s *podState) tree(all bool) *proto.Tree {
	s.mu.Lock()
	recs := make([]*execRec, 0, len(s.execs)+len(s.history))
	for _, r := range s.execs {
		recs = append(recs, r)
	}
	completed := len(s.history)
	if all {
		recs = append(recs, s.history...)
	}
	s.mu.Unlock()
	backends := map[string]string{}
	for _, info := range s.live() {
		backends[info.ID] = info.Backend
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Start.Before(recs[j].Start) })

	t := &proto.Tree{
		Pod:     s.name,
		Uptime:  time.Since(s.started).Truncate(time.Second).String(),
		Default: s.table().Default,
	}
	// The live mount list, in the order the mounts were added — which is the
	// order that decides what shadows what, so it is the order to show.
	for _, m := range s.mountList() {
		t.Mounts = append(t.Mounts, proto.TreeMount{At: m.At, Source: m.source(),
			Kind: m.Kind, Owner: m.Owner, ExecOn: m.ExecOn, ReadOnly: m.ReadOnly})
	}

	byPID := map[int]*proto.TreeNode{}
	for _, r := range recs {
		if r.done() && !all {
			continue
		}
		// With --all, a pid may appear more than once; the live program wins
		// the parenting, since that is what its children actually belong to.
		if prev, ok := byPID[r.PID]; ok && prev.State == "running" {
			continue
		}
		byPID[r.PID] = nodeOf(r)
	}
	// Attach each node to its parent when the parent is also tracked;
	// otherwise it is a root of its session.
	var roots []*proto.TreeNode
	for _, r := range recs {
		n, ok := byPID[r.PID]
		if !ok {
			continue
		}
		if parent, ok := byPID[r.PPID]; ok && r.PPID != r.PID {
			parent.Children = append(parent.Children, *n)
			continue
		}
		roots = append(roots, n)
	}
	// Completed work collapses to a count: an hour of agent work is hundreds
	// of execs, and an uncollapsed tree is unreadable in either language.
	t.Completed = completed
	sess := map[string]*proto.TreeSession{}
	for _, n := range roots {
		id := n.Session
		if id == "" {
			id = "detached"
		}
		if sess[id] == nil {
			sess[id] = &proto.TreeSession{ID: id, Backend: backends[id]}
		}
		sess[id].Nodes = append(sess[id].Nodes, *n)
	}
	// A session with nothing running is still a session, and which machine it is
	// on is the fact the tree exists to show.
	for id, backend := range backends {
		if sess[id] == nil {
			sess[id] = &proto.TreeSession{ID: id, Backend: backend}
		}
	}
	ids := make([]string, 0, len(sess))
	for id := range sess {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t.Sessions = append(t.Sessions, *sess[id])
	}
	return t
}

func nodeOf(r *execRec) *proto.TreeNode {
	state := "running"
	if r.done() {
		state = "exited"
	}
	return &proto.TreeNode{
		PID: r.PID, Argv: r.Argv, Target: r.Target, State: state,
		ElapsedMS: r.elapsedMS(), Code: r.Code, Session: r.Session,
		StartedMS: r.Start.UnixMilli(), ExecID: r.ExecID,
	}
}
