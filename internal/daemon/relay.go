package daemon

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/route"
)

// Relaying a directory to a node through this machine.
//
// A node pod normally mounts what it needs itself, from the machine that owns it.
// Two cases cannot work that way: a node that cannot reach the owner — node-to-node
// ssh is firewalled on many clusters — and a directory on *this* machine, which no
// node can reach at all. For both, this machine serves the directory with `rclone
// serve sftp` on its own loopback, and a reverse forward on the ssh connection
// vibepod already holds makes that a port on the node's loopback.
//
// What authenticates the node is a key made for that one tunnel: it grants that one
// directory, is read by nothing but that server, and dies with the pod. None of the
// user's own keys goes anywhere, which is the promise this whole design exists for.

// relayServer serves one directory of this machine to whichever nodes need it.
type relayServer struct {
	at   string
	src  string
	ro   bool
	port int
	dir  string // key material for this one server
	cmd  *exec.Cmd
}

// relayLink is one node's view of one relay: the port its loopback got.
type relayLink struct {
	remotePort int
	keyOnNode  string
}

// rcloneBin is the rclone that serves relays. VIBEPOD_RCLONE names one outside
// PATH, which a test needs and a machine with rclone installed somewhere odd wants.
func rcloneBin() (string, error) {
	if p := os.Getenv("VIBEPOD_RCLONE"); p != "" {
		return p, nil
	}
	p, err := exec.LookPath("rclone")
	if err != nil {
		return "", fmt.Errorf("relaying through this machine needs rclone here, to " +
			"serve the directory to the node, and there is none — install it, or " +
			"give that node its own path to the data (`via: direct`)")
	}
	return p, nil
}

// server starts, or returns, the relay for one mount.
func (s *podState) server(m *mountRec) (*relayServer, error) {
	s.mu.Lock()
	if srv := s.relays[m.At]; srv != nil {
		s.mu.Unlock()
		return srv, nil
	}
	s.mu.Unlock()
	bin, err := rcloneBin()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.relaySeq++
	dir := filepath.Join(s.runDir(), "relay", fmt.Sprintf("%d", s.relaySeq))
	s.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for _, k := range []string{"client", "host"} {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "",
			"-C", "vibepod relay "+m.At, "-f", filepath.Join(dir, k)).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("make a key for the relay: %v: %s", err, out)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	args := []string{"serve", "sftp", m.Src, "--addr", fmt.Sprintf("127.0.0.1:%d", port),
		"--user", "vp", "--authorized-keys", filepath.Join(dir, "client.pub"),
		"--key", filepath.Join(dir, "host")}
	if m.ReadOnly {
		args = append(args, "--read-only")
	}
	cmd := exec.Command(bin, args...)
	log, _ := os.Create(filepath.Join(dir, "serve.log"))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the relay for %s: %w", m.At, err)
	}
	for i := 0; ; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if i > 50 {
			_ = cmd.Process.Kill()
			out, _ := os.ReadFile(filepath.Join(dir, "serve.log"))
			return nil, fmt.Errorf("the relay for %s did not start: %s", m.At,
				strings.TrimSpace(string(out)))
		}
		time.Sleep(100 * time.Millisecond)
	}
	srv := &relayServer{at: m.At, src: m.Src, ro: m.ReadOnly, port: port, dir: dir, cmd: cmd}
	s.mu.Lock()
	if s.relays == nil {
		s.relays = map[string]*relayServer{}
	}
	s.relays[m.At] = srv
	s.mu.Unlock()
	s.d.logf("pod %s: relaying %s on 127.0.0.1:%d", s.name, m.At, port)
	return srv, nil
}

// relayFor gives one node a way to one relayed mount: the reverse forward, and the
// tunnel's key in that node's pod directory, readable by the user alone.
func (s *podState) relayFor(np *nodePod, m *mountRec) (*proto.RelaySpec, error) {
	srv, err := s.server(m)
	if err != nil {
		return nil, err
	}
	key := np.host + "\x00" + m.At
	s.mu.Lock()
	link := s.relayLinks[key]
	s.mu.Unlock()
	if link == nil {
		h := s.d.pool.Host(np.host)
		port, err := h.ReverseForward(srv.port)
		if err != nil {
			return nil, err
		}
		priv, err := os.ReadFile(filepath.Join(srv.dir, "client"))
		if err != nil {
			return nil, err
		}
		onNode := filepath.Join(np.home, ".vp", "run", s.nodeName(np.host),
			fmt.Sprintf("relay-%d.key", srv.port))
		if err := h.WriteFile(onNode, priv); err != nil {
			h.CancelReverse(port, srv.port)
			return nil, err
		}
		link = &relayLink{remotePort: port, keyOnNode: onNode}
		s.mu.Lock()
		if s.relayLinks == nil {
			s.relayLinks = map[string]*relayLink{}
		}
		s.relayLinks[key] = link
		s.mu.Unlock()
	}
	return &proto.RelaySpec{Port: link.remotePort, KeyFile: link.keyOnNode, User: "vp"}, nil
}

// via decides how a node reaches one mount, and says why when it relays.
//
// Asked of the node rather than guessed: whether gpu05 can ssh to gpu03 by itself
// depends on its config, its keys and the network between them, none of which this
// machine can see. The answer is remembered for the pair, so it costs one round
// trip per machine the node needs, not one per mount.
func (s *podState) via(host string, m *mountRec) string {
	np := &nodePod{host: host}
	if m.Owner == route.Pod {
		return "relay" // nothing else can reach this machine's own files
	}
	if m.Owner == np.host {
		return "direct" // its own disk; no network at all
	}
	switch m.Via {
	case "direct", "relay":
		return m.Via
	}
	key := np.host + "\x00" + m.Owner
	s.mu.Lock()
	decided, known := s.reach[key]
	s.mu.Unlock()
	if known {
		return decided
	}
	decided = "direct"
	s.mu.Lock()
	agent := s.agents[np.host]
	s.mu.Unlock()
	if err := s.d.pool.Host(np.host).CanReach(m.Owner, s.nodeSSHExtra(), agent); err != nil {
		decided = "relay"
		msg := fmt.Sprintf("%s → %s  direct: %v; relaying through this machine",
			np.host, m.Owner, err)
		s.d.logf("pod %s: %s", s.name, msg)
		s.d.bus.Publish(event.Event{Kind: event.KindNotice, Pod: s.name,
			Target: np.host, Detail: msg})
	}
	s.mu.Lock()
	if s.reach == nil {
		s.reach = map[string]string{}
	}
	s.reach[key] = decided
	s.mu.Unlock()
	return decided
}

// dropRelays closes one node's reverse forwards, when its pod goes.
func (s *podState) dropRelays(host string) {
	s.mu.Lock()
	var links []*relayLink
	var locals []int
	for key, l := range s.relayLinks {
		if strings.HasPrefix(key, host+"\x00") {
			links = append(links, l)
			at := strings.TrimPrefix(key, host+"\x00")
			if srv := s.relays[at]; srv != nil {
				locals = append(locals, srv.port)
			} else {
				locals = append(locals, 0)
			}
			delete(s.relayLinks, key)
		}
	}
	s.mu.Unlock()
	h := s.d.pool.Host(host)
	for i, l := range links {
		h.CancelReverse(l.remotePort, locals[i])
	}
}

// stopRelay ends one mount's relay everywhere, when the mount goes.
func (s *podState) stopRelay(at string) {
	s.mu.Lock()
	srv := s.relays[at]
	delete(s.relays, at)
	type pending struct {
		host string
		link *relayLink
	}
	var drop []pending
	for key, l := range s.relayLinks {
		if strings.HasSuffix(key, "\x00"+at) {
			drop = append(drop, pending{strings.SplitN(key, "\x00", 2)[0], l})
			delete(s.relayLinks, key)
		}
	}
	s.mu.Unlock()
	if srv == nil {
		return
	}
	for _, p := range drop {
		s.d.pool.Host(p.host).CancelReverse(p.link.remotePort, srv.port)
	}
	if srv.cmd != nil && srv.cmd.Process != nil {
		_ = srv.cmd.Process.Kill()
		_, _ = srv.cmd.Process.Wait()
	}
}

func (s *podState) stopRelays() {
	s.mu.Lock()
	ats := make([]string, 0, len(s.relays))
	for at := range s.relays {
		ats = append(ats, at)
	}
	s.mu.Unlock()
	for _, at := range ats {
		s.stopRelay(at)
	}
}
