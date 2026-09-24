package daemon

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"vibepod/internal/complete"
	"vibepod/internal/proto"
	"vibepod/internal/remote"
	"vibepod/internal/route"
)

// completeTimeout bounds a Tab. A completion that takes longer than this is
// worse than none: the line is still there to finish by hand.
const completeTimeout = 3 * time.Second

// complete asks the shell on the session's machine what it would offer at the
// end of m.Data, in m.Cwd — the directory that shell reported, so already a
// path on that side. It is not a command anyone ran, so it is not in `vp log`.
func (d *Daemon) complete(m *proto.Msg) (string, error) {
	s, err := d.lookup(m.Pod)
	if err != nil {
		return "", err
	}
	backend := s.backendOf(m.Session)
	s.mu.Lock()
	sess := s.sessions[m.Session]
	s.mu.Unlock()
	var base []string
	if sess != nil {
		if b := sess.backendOfShell(); b != "" {
			backend = b
		}
		base = sess.env
	}
	fpath := ""
	for _, e := range m.Env {
		if v, ok := strings.CutPrefix(e, "VP_FPATH="); ok {
			fpath = v
		}
	}
	argv, env := complete.Command(string(m.Data), fpath)

	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		r.Close()
		w.Close()
		return "", err
	}
	go func() {
		defer w.Close()
		defer null.Close()
		if backend == route.Pod {
			fds := []int{int(null.Fd()), int(w.Fd()), int(null.Fd())}
			_, _ = s.runToCompletion(&proto.Msg{Op: proto.OpSpawn, Argv: argv,
				Env: append(append([]string(nil), base...), env...), Cwd: m.Cwd}, fds)
			return
		}
		np := s.nodePods.get(backend)
		if np == nil {
			return
		}
		host := s.d.pool.Host(backend).InPod(np.home, s.nodeName(backend))
		req := remote.Req{Dir: m.Cwd, Argv: argv, Env: env,
			ID: fmt.Sprintf("%s-complete-%d", s.name, execSeq.Add(1))}
		req.Files = [3]*os.File{null, w, null}
		_, _ = host.Run(req)
	}()

	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(r, 1<<20))
		done <- b
	}()
	select {
	case b := <-done:
		r.Close()
		return string(b), nil
	case <-time.After(completeTimeout):
		r.Close()
		return "", fmt.Errorf("no completion from %s within %s", backend, completeTimeout)
	}
}
