package daemon

import (
	"fmt"
	"os"
	"path/filepath"
)

// The credential proxy: this machine's ssh agent, reachable on a node that the
// config explicitly trusts with it.
//
// It exists for one reason. A node pod mounting another machine's directory has to
// authenticate to that machine, and the promise this design is built on is that no
// key is ever copied anywhere. A forwarded agent keeps that promise — the key stays
// here and only signatures cross — but it is still authority lent to a machine: for
// as long as the connection lives, anything on that node running as you can use
// it. So it is off unless a host says `forward_credentials: true`, and it is scoped
// to that host alone.

// agentFor forwards the agent to a trusted machine, once, and returns the socket
// path on that machine. Empty, with no error, for a machine that is not trusted.
func (s *podState) agentFor(host, home string) (string, error) {
	s.mu.Lock()
	trusted := s.credentials[host]
	sock := s.agents[host]
	s.mu.Unlock()
	if !trusted {
		return "", nil
	}
	if sock != "" {
		return sock, nil
	}
	local := os.Getenv("SSH_AUTH_SOCK")
	if local == "" {
		return "", fmt.Errorf("%s is marked forward_credentials, but this daemon has "+
			"no ssh agent to forward (SSH_AUTH_SOCK is unset where it was started)", host)
	}
	// In the node pod's control directory, which that pod sees at /vp/run: a pod on
	// a node holds the composed zone and its own system layer, not the node's home,
	// so a socket anywhere else would be unreachable from the commands that need it.
	sock = filepath.Join(home, ".vp", "run", s.nodeName(host), "ctl", "agent.sock")
	if err := s.d.pool.Host(host).ForwardAgent(sock, local); err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.agents == nil {
		s.agents = map[string]string{}
	}
	s.agents[host] = sock
	s.mu.Unlock()
	s.d.logf("pod %s: %s may use this machine's ssh agent at %s", s.name, host, sock)
	return sock, nil
}

// homeOf is a machine's home directory, asked once. Only machines trusted with the
// agent need it, which is why this is not part of every connection.
func (s *podState) homeOf(host string) string {
	s.mu.Lock()
	trusted := s.credentials[host]
	if np := s.nodePods.get(host); np != nil {
		s.mu.Unlock()
		return np.home
	}
	home := s.homes[host]
	s.mu.Unlock()
	if !trusted {
		return ""
	}
	if home != "" {
		return home
	}
	out, err := s.d.pool.Host(host).Capture(`printf '%s' "$HOME"`)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	if s.homes == nil {
		s.homes = map[string]string{}
	}
	s.homes[host] = out
	s.mu.Unlock()
	return out
}

// podAgentSock is the same socket as a command inside that machine's node pod sees
// it.
const podAgentSock = "/vp/run/agent.sock"
