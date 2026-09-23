package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vibepod/internal/event"
)

// `mode: sync`: a local copy instead of a network mount.
//
// For a huge repository or a link with a long round trip, reading through FUSE
// costs a round trip per file the first time, and an editor or an agent reads a lot
// of files. A local copy costs one transfer up front and then nothing. The price is
// that there are two copies, and keeping them the same is the whole problem.
//
// They are kept the same the way the cache is kept honest: around execution. Before
// a command runs on the machine that owns the directory, local changes are pushed;
// after it finishes, its results are pulled. vibepod mediates every dispatched
// command, so it knows the only moments either side can have changed — no timer, no
// watcher, nothing polling.
//
// Deletions travel too, using a record of what the last sync saw: a file in that
// record and missing on one side was deleted there. A file changed on one side since
// the last sync and deleted on the other is a conflict, and it is kept and reported
// rather than resolved by guessing which side meant it.

type syncState struct {
	mu       sync.Mutex
	at       string
	host     string
	remote   string
	local    string
	manifest map[string]bool // relative paths both sides had after the last sync
	last     time.Time
}

// startSync makes the local copy and pulls it, at `up`.
func (s *podState) startSync(at, host, remote string, pr *Progress) (string, error) {
	s.mu.Lock()
	s.syncSeq++
	local := filepath.Join(s.runDir(), "sync", fmt.Sprintf("%d", s.syncSeq))
	s.mu.Unlock()
	if err := os.MkdirAll(local, 0o755); err != nil {
		return "", err
	}
	st := &syncState{at: at, host: host, remote: strings.TrimSuffix(remote, "/"),
		local: local, manifest: map[string]bool{}}
	pr.step("copying %s:%s here… ", host, remote)
	if err := s.pull(st); err != nil {
		pr.failed()
		return "", err
	}
	pr.ok("%d files", len(st.manifest))
	s.mu.Lock()
	if s.syncs == nil {
		s.syncs = map[string]*syncState{}
	}
	s.syncs[at] = st
	s.mu.Unlock()
	return local, nil
}

// syncsFor are the synced mounts a command on this machine could read or write:
// the ones it owns, and any its node pod holds.
func (s *podState) syncsFor(backend string) []*syncState {
	s.mu.Lock()
	all := make([]*syncState, 0, len(s.syncs))
	for _, st := range s.syncs {
		all = append(all, st)
	}
	s.mu.Unlock()
	np := s.nodePods.get(backend)
	var out []*syncState
	for _, st := range all {
		if st.host == backend || (np != nil && np.holds(st.at)) {
			out = append(out, st)
		}
	}
	return out
}

// beforeDispatch pushes local changes to a machine a command is about to run on.
func (s *podState) beforeDispatch(backend string) {
	for _, st := range s.syncsFor(backend) {
		if err := s.push(st); err != nil {
			s.syncNote(st, "push before running on "+backend+": "+err.Error())
		}
	}
}

// afterDispatch pulls what a command changed.
func (s *podState) afterDispatch(backend string) {
	for _, st := range s.syncsFor(backend) {
		if err := s.pull(st); err != nil {
			s.syncNote(st, "pull after running on "+backend+": "+err.Error())
		}
	}
}

// flushSyncs pushes everything, when the pod goes down: the copy here is about to
// be deleted, and whatever was edited in it exists nowhere else.
func (s *podState) flushSyncs() {
	s.mu.Lock()
	all := make([]*syncState, 0, len(s.syncs))
	for _, st := range s.syncs {
		all = append(all, st)
	}
	s.mu.Unlock()
	for _, st := range all {
		if err := s.push(st); err != nil {
			s.syncNote(st, "final push: "+err.Error())
		}
	}
}

func (s *podState) push(st *syncState) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	h := s.d.pool.Host(st.host)
	here, err := listLocal(st.local)
	if err != nil {
		return err
	}
	there, err := s.listRemote(st)
	if err != nil {
		return err
	}
	// Deleted here since the last sync: delete there — unless it changed there
	// since, which is a conflict to keep, not a deletion to apply.
	var gone []string
	for p := range st.manifest {
		if _, ok := here[p]; ok {
			continue
		}
		if mt, ok := there[p]; ok && mt.After(st.last) {
			s.syncNote(st, fmt.Sprintf("kept %s: deleted here, changed on %s", p, st.host))
			continue
		}
		gone = append(gone, p)
	}
	// And the other direction: in the record, still here, missing there. Deleted
	// over there — so delete it here rather than send it back — unless it changed
	// here since, in which case it is sent back and the conflict is said out loud.
	for p := range st.manifest {
		if _, ok := there[p]; ok {
			continue
		}
		mt, ok := here[p]
		if !ok {
			continue
		}
		if mt.After(st.last) {
			s.syncNote(st, fmt.Sprintf("kept %s: deleted on %s, changed here — sent back",
				p, st.host))
			continue
		}
		_ = os.Remove(filepath.Join(st.local, p))
		delete(here, p)
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		cmd := "cd " + shq(st.remote) + " && rm -f --"
		for _, p := range gone {
			cmd += " " + shq(p)
		}
		if _, err := h.Capture(cmd); err != nil {
			return err
		}
	}
	if err := s.rsync(st, st.local+"/", st.host+":"+st.remote+"/"); err != nil {
		return err
	}
	st.manifest = map[string]bool{}
	for p := range here {
		st.manifest[p] = true
	}
	st.last = time.Now()
	return nil
}

func (s *podState) pull(st *syncState) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	there, err := s.listRemote(st)
	if err != nil {
		return err
	}
	here, err := listLocal(st.local)
	if err != nil {
		return err
	}
	for p := range st.manifest {
		if _, ok := there[p]; ok {
			continue
		}
		if mt, ok := here[p]; ok && mt.After(st.last) {
			s.syncNote(st, fmt.Sprintf("kept %s: deleted on %s, changed here", p, st.host))
			continue
		}
		_ = os.Remove(filepath.Join(st.local, p))
	}
	if err := s.rsync(st, st.host+":"+st.remote+"/", st.local+"/"); err != nil {
		return err
	}
	st.manifest = map[string]bool{}
	for p := range there {
		st.manifest[p] = true
	}
	st.last = time.Now()
	return nil
}

// rsync copies newer files one way, on the multiplexed connection. --update means a
// file changed on both sides since the last sync keeps the newer copy.
func (s *podState) rsync(st *syncState, from, to string) error {
	h := s.d.pool.Host(st.host)
	out, err := exec.Command("rsync", "-a", "--update", "-e", h.SSHCommand(), from, to).
		CombinedOutput()
	if err != nil {
		return fmt.Errorf("rsync: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *podState) listRemote(st *syncState) (map[string]time.Time, error) {
	out, err := s.d.pool.Host(st.host).Capture("cd " + shq(st.remote) +
		" && find . -type f -printf '%T@\\t%P\\n'")
	if err != nil {
		return nil, err
	}
	files := map[string]time.Time{}
	for _, line := range strings.Split(out, "\n") {
		ts, p, ok := strings.Cut(line, "\t")
		if !ok || p == "" {
			continue
		}
		f, _ := strconv.ParseFloat(ts, 64)
		files[p] = time.Unix(int64(f), int64((f-float64(int64(f)))*1e9))
	}
	return files, nil
}

func listLocal(root string) (map[string]time.Time, error) {
	files := map[string]time.Time{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		files[rel] = fi.ModTime()
		return nil
	})
	return files, err
}

func (s *podState) syncNote(st *syncState, msg string) {
	s.d.logf("pod %s: sync %s: %s", s.name, st.at, msg)
	s.d.bus.Publish(event.Event{Kind: event.KindNotice, Pod: s.name, Target: st.host,
		Detail: "sync " + st.at + ": " + msg})
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
