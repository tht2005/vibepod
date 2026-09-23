# v1 implementation plan (M0-M2)

Companion to DESIGN.md. Records what was verified before building, the one design
change the evidence forced, and the build order with its gates.

## 0. Verified before writing code

Every load-bearing kernel assumption was spiked first (`scratchpad/spike*`).

| # | Assumption | Result |
|---|---|---|
| 1 | seccomp `NEW_LISTENER` installs unprivileged in a userns | ✓ with `TSYNC\|TSYNC_ESRCH\|NEW_LISTENER` |
| 2 | listener fd exports to a host supervisor over `SCM_RIGHTS` | ✓ |
| 3 | notify carries a pid usable by the host | ✓ translated into the reader's pidns |
| 4 | pathname readable from the frozen process | ✓ `/proc/pid/mem` at `args[0]`; yama=1 OK (pod is a descendant) |
| 5 | cwd readable at exec time | ✓ `/proc/pid/cwd`, correct after `cd` |
| 6 | bind-mount lands while the syscall is frozen, `CONTINUE` resolves the shim | ✓ **shim received original argv and cwd** |
| 7 | ...even over a path inside a read-only `/usr` | ✓ (native pod) |
| 8 | the pod can be built so children hold no capabilities | ✓ `CapEff: 0`, child `mount()` → EPERM |
| 9 | vpinit can still mount *after* dropping ambient caps | ✓ — lazy shims depend on this |
| 10 | bwrap can host all of the above | ✗ **see below** |

## 1. Forced change: native namespace setup replaces bubblewrap

`DESIGN.md` locked "Sandbox: bwrap, behind an interface". That is not implementable.

Inside a bwrap-constructed root **no mount of any kind succeeds**, with `CapEff` showing
a full capability set. `ioctl(NS_GET_USERNS)` on the pod's mount namespace returns EPERM,
which means the mount namespace is owned by a user namespace that is *not* ours: bwrap
creates a **second, nested userns** after building the root, precisely so the sandbox
cannot touch the mounts bwrap made for it. `--cap-add ALL` grants capabilities in the
inner namespace; `may_mount()` checks the outer one.

It is a good security property and it is fatal to lazy bind-shims, which require mounting
into the live pod. `--dev-bind / /` is the one shape that works (bwrap skips the second
userns when there is nothing to protect) and it defeats the `host_access` allowlist.

So vibepod builds its own namespace, as runc and podman do:

```
clone(CLONE_NEWUSER|CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWIPC)   identity uid map
  └ vpinit, holding CAP_SYS_ADMIN via the ambient set
      ├ tmpfs root, ro-binds, /proc, /dev, devpts, pivot_root
      ├ seccomp filter installed, listener fd exported to vibepod
      ├ PR_CAP_AMBIENT_CLEAR_ALL   → every descendant has CapEff 0
      └ retains CAP_SYS_ADMIN itself, and is the pod's only mount agent
```

Go's `SysProcAttr.AmbientCaps` does the capability hand-off, so there is no setuid helper,
no `newuidmap`, and no subuid range. Identity uid mapping means host files keep their
ownership inside the pod.

Two properties this buys that bwrap could not:

- the agent cannot mount, unmount, or unshim anything (verified EPERM)
- the daemon never needs `nsenter`, which loses capabilities on exec anyway

Cost: ~250 lines of mount plumbing we own, and bwrap's hardening is ours to re-derive.
The `pod.Sandbox` interface stays, so bwrap can return for pods that need no live mounts.

## 2. Other implementation decisions

- **Mounts are performed by `vpinit`**, on request from the daemon over the pod socket.
  `nsenter` from the daemon cannot work: entering the userns grants capabilities, and the
  subsequent `execve` drops them again for a non-root euid.
- **Shim stash.** Before shadowing `/usr/bin/cargo`, bind the original to
  `/vp/real/<name>` so `vpsh` can still run it for pod-local execs. Without this the
  shim shadows the binary it needs.
- **Two daemon sockets**, not one: `host.sock` (full control) and a per-pod socket bound
  into the namespace (read-only + tty-gated `use`). The capability table in DESIGN.md §9
  is enforced by which socket the connection arrived on, not by a flag.
- **FS backend.** `rclone` is not installed here; `sshfs` is. Ship a `fs.Backend`
  interface with both: sshfs by default, rclone when present (it alone supports
  execution-aware invalidation via `vfs/forget`). `vpctl doctor` reports which is active.
- **Remote signals.** The remote command runs as `sh -c 'echo $$ >run/<id>; exec <cmd>'`;
  after `exec` the pid is the command's own, so a second multiplexed channel can signal it.
- **Dependencies:** `gopkg.in/yaml.v3` only. PTY and termios are hand-rolled (~70 lines)
  rather than pulling a terminal library.

## 3. Build order and gates

Each gate is an automated test that must pass before the next stage starts.

**G0 — the trick works** (`internal/pod`, `internal/gate`, `internal/shim`)
- native pod, seccomp gate, lazy bind-shim, stash, `vpctl run`
- gate: a test binary run inside the pod is redirected to `vpsh`, which reports the
  original argv and cwd, then executes the stashed original and proxies its exit code

**G1 — remotes** (`internal/config`, `route`, `remote`, `fs`)
- `vibepod.yaml`, cwd routing, ssh ControlMaster pool, sshfs mount, remote exec with
  streamed stdio and forwarded signals
- gate: an integration test against a local sshd fixture on port 2222 — mount a directory,
  run a command whose cwd is inside it, assert it executed on the "remote"

**G2 — lifecycle** (`internal/daemon`, `event`, `term`, `ui`)
- daemon persistence, sessions, PTY + detach/attach, NDJSON event stream, `log`, `tree`,
  `ps`, `down`, the console
- gate: create a pod, run a command, detach, reattach and see the scrollback; `tree --json`
  reports the live exec with its target

**G3 — verify v1**
- run Claude Code inside a pod with every exec intercepted and cwd tracking intact
- `doctor`, failure injection (link drop → exit 75), shim accumulation check
