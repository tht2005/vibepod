// Package proto carries every message that crosses a vibepod socket.
//
// Three links use it:
//
//	vp      -> vibepod   over the host socket (full control)
//	vp/vpsh -> vibepod   over the pod socket  (dispatch, and the exec log)
//	vibepod <-> vpinit   over an inherited socketpair (mounts and spawns)
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
	OpBind   = "bind"   // bind Src over Dst, now or long after up
	OpUnbind = "unbind" // detach a bind the pod no longer wants
	OpSpawn  = "spawn"  // start a process in the pod, using the passed fds
	OpSignal = "signal" // deliver Sig to Pid

	// vpinit -> daemon
	OpHello   = "hello"   // the pod exists; Pid is vpinit's own
	OpSpawned = "spawned" // Pid of a started process
	OpExited  = "exited"  // a pod process finished

	// vpsh -> daemon
	OpExec = "exec" // this command line is about to run in the pod

	// vp -> daemon
	OpDispatch = "dispatch" // run Argv on Backend, on the passed fds
	OpBackend  = "backend"  // which backend is this session on?
	OpLog      = "log"
	OpTree     = "tree"
	OpEvent    = "event"
	OpEnd      = "end"
	OpUse      = "use"       // move a session's backend
	OpHosts    = "hosts"     // the machines this pod knows, mounted or not
	OpStat     = "stat"      // does this path exist in the pod, and is it a directory?
	OpMount    = "mount"     // connect and mount into a running pod
	OpUnmount  = "unmount"   // unmount and disconnect
	OpSave     = "save"      // hand back the live state, for vibepod.yaml
	OpForward  = "forward"   // forward a port, or list the forwards
	OpNodeAdd  = "node-add"  // build a pod on a machine
	OpNodeDrop = "node-drop" // stop one and remove its state
	OpNodeList = "node-list" // the machines with pods, and what they hold

	// daemon -> vpnode, over ssh
	OpHeld       = "held"       // what does this node pod hold?
	OpInvalidate = "invalidate" // forget what you cached for this mount
	OpLease      = "lease"      // the daemon is still here
	OpBrief      = "brief"      // the generated agent brief, as the pod sees it
	OpUp         = "up"
	OpPs         = "ps"
	OpDown       = "down"
	OpSession    = "session" // attach a terminal and run Argv in the pod
	OpAttach     = "attach"  // reconnect to a session the daemon is holding
	OpWinch      = "winch"   // the client's terminal was resized
	OpDetach     = "detach"  // left running, deliberately
	OpInput      = "input"   // keystrokes for an attached session

	// OpProgress is sent while a request is still being worked on, so that a
	// wait long enough to look like a hang does not have to be one.
	OpProgress = "progress"

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
	Name     string `json:"name"`
	Root     string `json:"root"`      // host dir that becomes the pod root
	RunDir   string `json:"run_dir"`   // host dir bound in at /vp/run
	ShellBin string `json:"shell_bin"` // host path of vpsh, the pod's $SHELL
	CtlBin   string `json:"ctl_bin"`   // host path of the vibepod binary
	// StageDir is the host directory the daemon makes remote mounts in. It is
	// bound into the pod as a staging area so that a mount made an hour after
	// `up` can be reached from inside — see pod.StageDir.
	StageDir string `json:"stage_dir,omitempty"`
	// Tools get a three-line wrapper in /vp/bin, which leads PATH. Two kinds
	// are named there: commands that exist only on a remote, and commands the
	// agent should not run locally even though it could.
	Tools    []string `json:"tools,omitempty"`
	Binds    []Bind   `json:"binds"`
	Env      []string `json:"env,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
	// Brief is the generated agent instruction file (§9). With no exec gate it
	// is the primary mechanism rather than a nicety: an agent that has not been
	// told about `vp` runs everything in the pod, over FUSE.
	Brief string `json:"brief,omitempty"`
}

// EnvPolicy says how much of a caller's environment a routed command carries.
// Blanket forwarding is refused: a pod's environment holds the credentials this
// whole design exists to keep on one machine, and describes this machine rather
// than the one the command is going to.
type EnvPolicy struct {
	Mode  string   `json:"mode"`            // delta | none | explicit
	Names []string `json:"names,omitempty"` // for explicit
}

// MountSpec is one mount, as the client resolved it from the config — or as
// `vp mount` asked for it, which is the same message on purpose.
//
// The list is ordered, and the order is kept: it decides what shadows what, so
// it is part of the pod's state rather than an accident of how the file was
// written.
//
// A remote mount is made on the host and bound in. That is forced — setuid is
// inert inside a pod and fusermount3 is setuid — but it yields a property worth
// having: nothing in the pod can unmount, remount or tamper with a mount.
type MountSpec struct {
	At       string `json:"at"`
	Src      string `json:"src,omitempty"`  // local: the host directory
	Host     string `json:"host,omitempty"` // remote: the machine
	Path     string `json:"path,omitempty"` // remote: what that machine calls it
	ReadOnly bool   `json:"ro,omitempty"`
	Mode     string `json:"mode,omitempty"`
	// ExecOn is a suggestion: "this directory is meant to run on that machine".
	// It is surfaced by `vp hosts`, the tree and the agent brief, and it never
	// moves a command by itself. §5.
	ExecOn string `json:"exec_on,omitempty"`
	// Identity marks the agent's own configuration and credentials, bound in
	// from the host. They are the one plane that never leaves this machine, so
	// they are never offered to another node and never listed as a mount a
	// remote could have.
	Identity bool `json:"identity,omitempty"`
	// Requires are paths a machine must have before it may run this mount's work
	// — `/opt/rocm`, `/dev/kfd`. Checked at `up`, which turns "is this node
	// actually equivalent?" from something a failed job tells you into something
	// a question answers.
	Requires []string `json:"requires,omitempty"`
	// Cache bounds the on-disk cache a node keeps for this mount.
	Cache string `json:"cache,omitempty"`
	// Prefetch copies the tree up front rather than warming it lazily.
	Prefetch bool `json:"prefetch,omitempty"`
	// Generation is the version of the mount list this entry belongs to, so a node
	// still holding an older definition of the same path is a fact rather than a
	// guess. A path can be unmounted and remounted from a different machine.
	Generation int64 `json:"gen,omitempty"`
	// ManyWriters lets several machines write into this mount at once, accepting
	// that the last flush of any one file wins. Off by default: one write-back
	// cache per mount is the only coherence vibepod can enforce (see isWriter).
	ManyWriters bool `json:"many_writers,omitempty"`
	// Via is how a node pod reaches this mount's bytes: "direct" (its own ssh to
	// the owner), "relay" (through this machine), or "auto" (try direct, fall back,
	// say which). Empty means auto.
	Via string `json:"via,omitempty"`
	// ExposeTo is, for a directory on this machine, the machines allowed to see it.
	// Sending local files to a remote machine is a decision about where data goes,
	// so it is opt-in per mount and per machine.
	ExposeTo []string `json:"expose_to,omitempty"`
	// Relay, set in what a node is sent, says where the relayed copy is: a port on
	// that node's loopback and a key made for that one tunnel.
	Relay *RelaySpec `json:"relay,omitempty"`
}

// RelaySpec is one relayed mount as a node sees it.
type RelaySpec struct {
	Port    int    `json:"port"`
	KeyFile string `json:"key_file"` // on the node, mode 0600
	User    string `json:"user"`
}

// PortSpec is one forwarded port: Local on this machine reaches Remote on Host's
// own loopback.
type PortSpec struct {
	Host   string `json:"host"`
	Remote int    `json:"remote"`
	Local  int    `json:"local"`
}

// NodeSpec is what a node needs to build its own pod: the composed zone, and
// nothing else.
//
// The identity plane is absent by construction — there is no field for it. A node
// pod is filesystem-uniform and credential-free, which is the whole reason it is
// safe to run one on a machine you do not control.
type NodeSpec struct {
	Pod    string `json:"pod"`
	Node   string `json:"node"`    // this machine's alias, so it knows what it owns
	Root   string `json:"root"`    // becomes the node pod's root
	RunDir string `json:"run_dir"` // holds the socket, the stage and the state
	// Mounts is the composed zone in config order. A mount this node owns is a
	// native bind — no FUSE, no cache, no round trip; the rest are mounted from
	// the machines that own them.
	Mounts []MountSpec `json:"mounts"`
	// SSHExtra are options for this node's *own* outbound ssh, when it has to
	// reach a third machine for a mount it does not own.
	SSHExtra []string `json:"ssh_extra,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
	// Cache bounds the on-node disk cache, as rclone spells it (e.g. "200G").
	Cache string `json:"cache,omitempty"`
	// AgentSock, when set, is a socket on this node that reaches the ssh agent of
	// the machine that owns the pod. The node's own outbound ssh uses it; no key
	// is ever copied. Present only for machines the config marked
	// forward_credentials.
	AgentSock string `json:"agent_sock,omitempty"`
	// Lease is how long this pod outlives silence from the daemon before it
	// flushes, unmounts and exits. Empty means forever, which is right for a
	// machine you own and wrong for one you share.
	Lease string `json:"lease,omitempty"`
	// Version is the build that wrote this spec, so a node running an older
	// pushed binary is noticed rather than debugged.
	Version string `json:"version,omitempty"`
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
	// Framed asks for a session's output with vibepod's markers left in and
	// banners replaced by markers: the client is `vp shell`, which parses the
	// stream into command blocks rather than showing it raw.
	Framed bool `json:"framed,omitempty"`
	// Backend is the machine a session's commands run on: "pod", or an ssh
	// alias. Per-session, never global.
	Backend string `json:"backend,omitempty"`
	// Tool is a name from /vp/bin whose machine the daemon resolves, because
	// a wrapper cannot know which session invoked it.
	Tool string `json:"tool,omitempty"`

	Src      string `json:"src,omitempty"`
	Dst      string `json:"dst,omitempty"`
	ReadOnly bool   `json:"ro,omitempty"`

	AllocPTY bool `json:"alloc_pty,omitempty"`
	Rows     int  `json:"rows,omitempty"`
	Cols     int  `json:"cols,omitempty"`
	// Data carries terminal input, which is keystrokes: small enough that the
	// socket is the right place for it, while output stays on a passed
	// descriptor where the bulk belongs.
	Data []byte `json:"data,omitempty"`

	Pid  int `json:"pid,omitempty"`
	Sig  int `json:"sig,omitempty"`
	Code int `json:"code,omitempty"`

	Mounts    []MountSpec `json:"mounts,omitempty"`
	EnvPolicy *EnvPolicy  `json:"env_policy,omitempty"`
	// ToolHosts maps a name in /vp/bin to the machine that has it, for the
	// tools where the config said which.
	ToolHosts map[string]string `json:"tool_hosts,omitempty"`
	// CanMount is what `vp mount` may reach from inside the pod. Mounting
	// opens a network path out of a sandbox built for containment, which is
	// the one thing §9 does not leave unrestricted.
	CanMount []string `json:"can_mount,omitempty"`
	// Config is the path the pod was created from, so `vp save` writes back to
	// the file it came from rather than guessing.
	Config string `json:"config,omitempty"`
	// Node carries a node pod's spec, and Nodes the machines a client has
	// consented to push vibepod to.
	Node  *NodeSpec `json:"node,omitempty"`
	Nodes []string  `json:"nodes,omitempty"`
	// Machines are the backends this pod knows about whether or not they own a
	// mount. A compute node with no data of its own is the case that needs this:
	// it is named under `hosts:` and is a machine you dispatch to.
	Machines []string `json:"machines,omitempty"`
	// Credentials are the machines the config allowed to use this machine's ssh
	// agent (forward_credentials: true). Everything else gets nothing.
	Credentials []string `json:"credentials,omitempty"`
	// Toolbin are the machines vibepod may copy a missing static tool to.
	Toolbin []string `json:"toolbin,omitempty"`
	// Held is what a node pod reports it has, and Generation the desired state's
	// version. Together they are the whole of the reconciler's input.
	Held       []Held `json:"held,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	// Ports are forwarded ports: asked for at `up` or by `vp forward`, and
	// reported back by it.
	Ports []PortSpec `json:"ports,omitempty"`

	// Detail is human-facing text: what a slow request is waiting for, or why
	// it stopped waiting.
	Detail string `json:"detail,omitempty"`
	// Partial marks progress text that the next message completes, so a step
	// and its outcome land on one line.
	Partial bool `json:"partial,omitempty"`

	Follow bool            `json:"follow,omitempty"`
	All    bool            `json:"all,omitempty"`
	Tree   *Tree           `json:"tree,omitempty"`
	Event  json.RawMessage `json:"event,omitempty"`

	// Version identifies the build at each end. A daemon outlives the binary
	// that started it, so an upgraded client can find itself talking to the
	// old one.
	Version string `json:"version,omitempty"`

	Pods  []PodInfo  `json:"pods,omitempty"`
	Hosts []HostInfo `json:"hosts,omitempty"`
	// NodeInfos are the pods this pod has on other machines.
	NodeInfos []NodeInfo `json:"node_pods,omitempty"`
}

// Tree is the pod's structure: mounts and live execs, in one view.
type Tree struct {
	Pod       string        `json:"pod"`
	Uptime    string        `json:"uptime"`
	Default   string        `json:"default"` // the backend a new session opens on
	Mounts    []TreeMount   `json:"mounts"`
	Sessions  []TreeSession `json:"sessions"`
	Completed int           `json:"completed"`
}

type TreeMount struct {
	At       string `json:"at"`
	Source   string `json:"source"`
	Kind     string `json:"kind"`
	Owner    string `json:"owner"`
	ExecOn   string `json:"exec_on,omitempty"`
	ReadOnly bool   `json:"ro,omitempty"`
}

type TreeSession struct {
	ID      string     `json:"id"`
	Kind    string     `json:"kind,omitempty"`
	Backend string     `json:"backend,omitempty"`
	Nodes   []TreeNode `json:"nodes,omitempty"`
}

type TreeNode struct {
	PID       int        `json:"pid"`
	Argv      []string   `json:"argv"`
	Target    string     `json:"target"`
	State     string     `json:"state"`
	ElapsedMS int64      `json:"elapsed_ms"`
	Code      *int       `json:"code,omitempty"`
	Session   string     `json:"session,omitempty"`
	Children  []TreeNode `json:"children,omitempty"`
	// StartedMS is when it started, in Unix milliseconds, so `--since` can ask
	// about age rather than duration.
	StartedMS int64  `json:"started_ms,omitempty"`
	ExecID    string `json:"exec_id,omitempty"`
	// Remote is what `vp tree -x` found running under a dispatched command on the
	// machine it went to. Polled and approximate: vibepod sees what it dispatched,
	// not what that spawned, and does not pretend otherwise.
	Remote []RemoteProc `json:"remote,omitempty"`
}

// RemoteProc is one process seen under a dispatched command by polling `ps`.
type RemoteProc struct {
	PID  int    `json:"pid"`
	Args string `json:"args"`
}

// HostInfo is one machine a pod can run commands on. The pod itself counts as
// one: "here" is an answer to "where does this run", and leaving it out of the
// list would make the local case look like an absence rather than a choice.
type HostInfo struct {
	Name      string   `json:"name"`
	Local     bool     `json:"local,omitempty"`
	Connected bool     `json:"connected,omitempty"`
	Mounted   bool     `json:"mounted,omitempty"`
	Dirs      []string `json:"dirs,omitempty"`
	Default   bool     `json:"default,omitempty"`
}

// PodInfo is one row of vp ps.
type PodInfo struct {
	Name     string        `json:"name"`
	Pid      int           `json:"pid"`
	Sessions int           `json:"sessions"`
	Mounts   int           `json:"mounts"`
	Uptime   string        `json:"uptime"`
	Default  string        `json:"default,omitempty"`
	Live     []SessionInfo `json:"live,omitempty"`
	// Behind is, per machine, the mounts its node pod does not hold yet.
	Behind map[string][]string `json:"behind,omitempty"`
}

// NodeInfo is one machine with a pod on it: what it holds, which of those it owns
// outright rather than caching, and where it is behind.
type NodeInfo struct {
	Host   string `json:"host"`
	Mounts []Held `json:"mounts"`
	// Generation is the mount list this node has converged to. Lower than the
	// pod's own means it is behind, and on what is in Behind.
	Generation int64    `json:"generation"`
	Behind     []string `json:"behind,omitempty"`
	Reachable  bool     `json:"reachable"`
}

// Held is one mount as a node pod holds it. Identity, not just a path: a path can
// be unmounted and remounted from a different machine, and a node still holding
// the old one is exactly the case a generation exists to catch.
type Held struct {
	At     string `json:"at"`
	Host   string `json:"host,omitempty"`
	Path   string `json:"path,omitempty"`
	Native bool   `json:"native,omitempty"`
	// ReadOnly is set when this node is not the writer for the mount. §6a: one
	// write-back cache per mount, because two of them means two nodes holding
	// dirty copies of one file and the last flush winning silently.
	ReadOnly bool `json:"ro,omitempty"`
	// Generation this mount entered the desired list at.
	Generation int64 `json:"gen,omitempty"`
	// Relayed is set when the bytes come through this machine rather than from the
	// owner directly.
	Relayed bool `json:"relayed,omitempty"`
	// Hop marks a node's own directory that arrived after the pod was built, and
	// so goes through a local FUSE hop instead of a bind: nothing unprivileged can
	// place a bind into a namespace that already exists. A rebuild makes it native.
	Hop bool `json:"hop,omitempty"`
}

// SessionInfo is one session and the machine it is on — the fact `ps` exists to
// report, now that the machine is chosen rather than inferred.
type SessionInfo struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind,omitempty"`
	Backend string   `json:"backend"`
	Framed  bool     `json:"framed,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	Uptime  string   `json:"uptime,omitempty"`
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

// PeerPID is the pid of the process at the other end, translated into this
// process's pid namespace by the kernel.
//
// A pod has its own pid namespace, so a process inside it cannot report a
// number the daemon could use. Asking the kernel also means the pid is not
// self-reported, which matters for anything the agent can reach.
func (k *Conn) PeerPID() int {
	raw, err := k.c.SyscallConn()
	if err != nil {
		return 0
	}
	var pid int
	_ = raw.Control(func(fd uintptr) {
		if cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET,
			syscall.SO_PEERCRED); err == nil {
			pid = int(cred.Pid)
		}
	})
	return pid
}

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
