package daemon

import (
	"sort"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/sys"
)

// execRec is one observed program launch. The exec gate sees every one of
// them, which is why vibepod can answer "what is running, and where" in a way
// pstree cannot: pstree stops at the machine boundary.
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

func (s *podState) recordExec(r *execRec) {
	s.mu.Lock()
	s.execs[r.PID] = r
	if len(s.execs) > maxExecs {
		s.evictLocked()
	}
	s.mu.Unlock()
	s.d.bus.Publish(event.Event{
		Kind: event.KindExec, Pod: s.name, PID: r.PID, PPID: r.PPID,
		Argv: r.Argv, Cwd: r.Cwd, Target: r.Target, Session: r.Session,
	})
}

// retarget corrects a record once vpsh reports where the command actually
// went. The gate has to decide before the process has an identity; vpsh knows
// the session, and therefore any pin.
func (s *podState) retarget(pid int, target string) {
	s.mu.Lock()
	if r, ok := s.execs[pid]; ok {
		r.Target = target
	}
	s.mu.Unlock()
}

func (s *podState) evictLocked() {
	done := make([]*execRec, 0, len(s.execs))
	for _, r := range s.execs {
		if r.done() {
			done = append(done, r)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].End.Before(done[j].End) })
	for i := 0; i < len(done)/2; i++ {
		delete(s.execs, done[i].PID)
	}
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
		for _, r := range s.execs {
			if !r.done() && !sys.Alive(r.PID) {
				r.End = time.Now()
				finished = append(finished, r)
			}
		}
		s.mu.Unlock()
		// The log's contract is chronological. Exits noticed in the same tick
		// arrive from a map, so order them by when they started.
		sort.Slice(finished, func(i, j int) bool {
			return finished[i].Start.Before(finished[j].Start)
		})
		for _, r := range finished {
			s.d.bus.Publish(event.Event{
				Kind: event.KindExit, Pod: s.name, PID: r.PID, Argv: r.Argv,
				Target: r.Target, Code: r.Code, ElapsedMS: r.elapsedMS(),
				Session: r.Session,
			})
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
	recs := make([]*execRec, 0, len(s.execs))
	for _, r := range s.execs {
		recs = append(recs, r)
	}
	sessions := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		sessions = append(sessions, id)
	}
	s.mu.Unlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].Start.Before(recs[j].Start) })
	sort.Strings(sessions)

	t := &proto.Tree{
		Pod:         s.name,
		Uptime:      time.Since(s.started).Truncate(time.Second).String(),
		ExecDefault: s.table.Default,
	}
	for _, b := range s.p.Spec.Binds {
		m := proto.TreeMount{At: b.Dst, Source: b.Src, Kind: "bind",
			ReadOnly: b.ReadOnly, Target: "pod"}
		t.Mounts = append(t.Mounts, m)
	}
	if s.fs != nil {
		for i, fm := range s.fs.Mounts() {
			// Remote mounts were appended to Binds in the same order.
			idx := len(t.Mounts) - len(s.fs.Mounts()) + i
			if idx >= 0 && idx < len(t.Mounts) {
				t.Mounts[idx].Kind = s.fs.Backend()
				t.Mounts[idx].Source = fm.Host + ":" + fm.RemotePath
				t.Mounts[idx].Target = fm.Host
			}
		}
	}
	for _, r := range s.table.Rules() {
		if r.Target == "pod" {
			continue
		}
		for i := range t.Mounts {
			if t.Mounts[i].At == r.Prefix {
				t.Mounts[i].Target = r.Target
			}
		}
	}

	byPID := map[int]*proto.TreeNode{}
	for _, r := range recs {
		if r.done() && !all {
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
	completed := 0
	for _, r := range recs {
		if r.done() {
			completed++
		}
	}
	t.Completed = completed
	sess := map[string]*proto.TreeSession{}
	for _, n := range roots {
		id := n.Session
		if id == "" {
			id = "detached"
		}
		if sess[id] == nil {
			sess[id] = &proto.TreeSession{ID: id}
		}
		sess[id].Nodes = append(sess[id].Nodes, *n)
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
	}
}
