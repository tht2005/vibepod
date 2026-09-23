// Package proto carries every message that crosses a vibepod socket.
//
// Three links use it:
//
//	vpctl  -> vibepod   over the host socket (full control)
//	vpsh   -> vibepod   over the pod socket  (routing a single exec)
//	vibepod <-> vpinit  over an inherited socketpair (mounts and spawns)
//
// Sockets are SOCK_SEQPACKET so that one message is one datagram, which keeps
// passed file descriptors attached to the message they belong to.
package proto

import (
	"encoding/json"
	"fmt"
	"net"
	"syscall"
)

// Ops. Kept as strings so a stuck socket can be read with a hex dump.
const (
	// daemon -> vpinit
	OpInit   = "init"   // build the root from Spec
	OpBind   = "bind"   // bind Src over Dst (the shim mechanism)
	OpSpawn  = "spawn"  // start a process in the pod, using the passed fds
	OpSignal = "signal" // deliver Sig to Pid

	// vpinit -> daemon
	OpHello   = "hello"   // carries the seccomp listener fd
	OpSpawned = "spawned" // Pid of a started process
	OpExited  = "exited"  // a pod process finished

	// vpsh -> daemon
	OpExec = "exec" // where should this command run?

	// daemon -> vpsh
	OpRunLocal = "run-local" // exec Path yourself; the fds are already right

	// vpctl -> daemon
	OpUp      = "up"
	OpPs      = "ps"
	OpDown    = "down"
	OpSession = "session" // attach a terminal and run Argv in the pod

	// generic replies
	OpOK   = "ok"
	OpErr  = "err"
	OpExit = "exit"
)

// Bind is one mount to construct in the pod.
type Bind struct {
	Src      string `json:"src"`
	Dst      string `json:"dst"`
	ReadOnly bool   `json:"ro,omitempty"`
}

// Spec is everything vpinit needs to build a pod. It crosses the socketpair
// once, before the pod exists.
type Spec struct {
	Name     string   `json:"name"`
	Root     string   `json:"root"`     // host dir that becomes the pod root
	RunDir   string   `json:"run_dir"`  // host dir bound in at /vp/run
	ShimBin  string   `json:"shim_bin"` // host path of vpsh
	Binds    []Bind   `json:"binds"`
	Env      []string `json:"env,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
}

// Route is one cwd-to-machine rule, as resolved by vpctl from the config.
type Route struct {
	Prefix       string `json:"prefix"`
	Target       string `json:"target"`
	RemotePrefix string `json:"remote_prefix,omitempty"`
}

// RemoteMount is a directory on another machine to mount on the host and bind
// into the pod. FUSE cannot be mounted from inside: the pod has no
// capabilities and setuid is inert there, which is also why the agent cannot
// tamper with a mount.
type RemoteMount struct {
	Host     string `json:"host"`
	Path     string `json:"path"`
	At       string `json:"at"`
	ReadOnly bool   `json:"ro,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

// Msg is the single envelope for every link. Fields are shared rather than
// namespaced per op: the protocol is small and one struct keeps the codec
// trivial to read in a packet capture.
type Msg struct {
	Op  string `json:"op"`
	ID  uint64 `json:"id,omitempty"`
	Err string `json:"err,omitempty"`

	Spec *Spec `json:"spec,omitempty"`

	Pod     string   `json:"pod,omitempty"`
	Session string   `json:"session,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	Env     []string `json:"env,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Path    string   `json:"path,omitempty"`
	Target  string   `json:"target,omitempty"`
	TTY     bool     `json:"tty,omitempty"`

	Src      string `json:"src,omitempty"`
	Dst      string `json:"dst,omitempty"`
	ReadOnly bool   `json:"ro,omitempty"`

	Pid  int `json:"pid,omitempty"`
	Sig  int `json:"sig,omitempty"`
	Code int `json:"code,omitempty"`

	Routes      []Route       `json:"routes,omitempty"`
	Remotes     []RemoteMount `json:"remotes,omitempty"`
	ExecDefault string        `json:"exec_default,omitempty"`
	ShimAll     bool          `json:"shim_all,omitempty"`

	Pods []PodInfo `json:"pods,omitempty"`
}

// PodInfo is one row of vpctl ps.
type PodInfo struct {
	Name     string `json:"name"`
	Pid      int    `json:"pid"`
	Sessions int    `json:"sessions"`
	Mounts   int    `json:"mounts"`
	Uptime   string `json:"uptime"`
}

const maxMsg = 1 << 16

// Conn is a message-oriented connection that can carry file descriptors.
type Conn struct {
	c *net.UnixConn
}

func NewConn(c *net.UnixConn) *Conn { return &Conn{c: c} }

func Dial(path string) (*Conn, error) {
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, err
	}
	return &Conn{c: c}, nil
}

func Listen(path string) (*net.UnixListener, error) {
	return net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
}

// FromFD adopts an already-connected socket, such as the socketpair vpinit
// inherits from the daemon.
func FromFD(fd int, name string) (*Conn, error) {
	f := osNewFile(fd, name)
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("fd %d is not a unix socket", fd)
	}
	return &Conn{c: uc}, nil
}

// Send writes one message, optionally handing over file descriptors.
func (k *Conn) Send(m *Msg, fds ...int) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var oob []byte
	if len(fds) > 0 {
		oob = syscall.UnixRights(fds...)
	}
	_, _, err = k.c.WriteMsgUnix(b, oob, nil)
	return err
}

// Recv reads one message and any descriptors that came with it. The caller
// owns the returned fds.
func (k *Conn) Recv() (*Msg, []int, error) {
	buf := make([]byte, maxMsg)
	oob := make([]byte, 256)
	n, oobn, _, _, err := k.c.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, nil, err
	}
	var fds []int
	if oobn > 0 {
		scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return nil, nil, fmt.Errorf("parse control message: %w", err)
		}
		for _, scm := range scms {
			got, err := syscall.ParseUnixRights(&scm)
			if err != nil {
				continue
			}
			fds = append(fds, got...)
		}
	}
	var m Msg
	if err := json.Unmarshal(buf[:n], &m); err != nil {
		closeAll(fds)
		return nil, nil, fmt.Errorf("decode %q: %w", truncate(buf[:n]), err)
	}
	return &m, fds, nil
}

func (k *Conn) Close() error { return k.c.Close() }

// Errorf replies with an error carrying the request id.
func (k *Conn) Errorf(id uint64, format string, a ...any) error {
	return k.Send(&Msg{Op: OpErr, ID: id, Err: fmt.Sprintf(format, a...)})
}

func truncate(b []byte) string {
	if len(b) > 120 {
		return string(b[:120]) + "..."
	}
	return string(b)
}

func closeAll(fds []int) {
	for _, fd := range fds {
		syscall.Close(fd)
	}
}
