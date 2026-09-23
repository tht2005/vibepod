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

## 3. Build order and gates — all passed

Each gate is an automated test in `test/`. They create user namespaces, install
seccomp filters, run a real sshd on a high port, and drive real terminals,
because the parts of vibepod worth doubting are exactly the parts a mock would
not exercise.

**G0 — the trick works.** A binary run in the pod is redirected to `vpsh`,
which reports the original argv and cwd and then runs the stashed original.
Also: pod processes hold `CapEff: 0` and their `mount()` returns EPERM, so the
agent cannot unpick its own routing.

**G1 — remotes.** Against the sshd fixture: a command whose cwd is a remote
mount runs over ssh, one in a local directory does not, remote files read
through the mount, and a file written by a routed command is visible afterwards.
An unreachable host exits 75 with an error naming vibepod.

**G2 — lifecycle.** Detach with `Ctrl-\`, reattach, and the scrollback replays
while the session is still live. Several sessions on one pod with independent
working directories. `tree --json` parses; the log shows each command's machine
and the exit status of the routed ones. The console runs commands, names their
routes, and keeps the keyboard afterwards.

**G3 — verify v1.** `claude -p` produces **byte-identical output inside a pod
and outside it**, with every exec intercepted — which is the claim M0 existed
to test.

## 4. What verification found

Six real bugs, none of which a unit test would have reached:

1. **A read-only remount must repeat the flags the userns locked**, or EPERM.
   runc does this; we now read them back from `mountinfo` first.
2. **Capabilities are per-thread**, and Go forks from whichever thread it
   likes. Clearing the ambient set on one goroutine left others able to hand
   `CAP_SYS_ADMIN` to the agent. `vpinit` now pins a mounter thread that keeps
   the capability and a spawner thread that has none.
3. **A spawn/bind deadlock.** vpinit waited for the spawn it was performing,
   but that spawn's `execve` traps to the daemon, which may need vpinit to bind
   a shim first. Every earlier test missed it because a shell is never shimmed:
   only a session whose *first* program needs one deadlocks.
4. **No DNS in the pod.** `/etc/resolv.conf` is a symlink into `/run` on a
   systemd-resolved machine. The symptom was an agent that started perfectly
   and then timed out on its first API call.
5. **The daemon must not read a client's terminal.** A read blocked on a tty
   does not reliably return when the descriptor is closed, so the daemon went
   on eating keystrokes after a session ended. Input is now messages; output
   stays on a passed descriptor.
6. **`up` was not atomic.** Two terminals opening the same project each built a
   whole pod and one was orphaned, with a session attached to it.

And two things that were not bugs but were *dishonest*, which matters more for
a tool whose pitch is "your agent runs commands on prod":

- the log reported a route for commands that were never shimmed, so a shell —
  which always runs locally — was recorded as running on the remote
- it printed a tick for exits it never observed; vibepod is not the parent of a
  pod-local exec, so that status is now absent rather than invented

## 5. Deviations from DESIGN.md

- **`vpsh` is a separate binary.** argv[0] dispatch cannot work for a shim: it
  is never invoked under its own name. It identifies itself by
  `/proc/self/exe`, which resolves through the bind mount.
- **Session pins live in the daemon**, keyed by an inherited `VIBEPOD_SESSION`
  token, rather than in a `VIBEPOD_EXEC` environment variable. A process cannot
  set its parent's environment, so `vpctl use` could not have worked that way.
- **sshfs ships as a backend**, not only rclone, because rclone is not
  installed here and requiring it would block the whole M1 path. rclone is
  preferred when present; `doctor` says which is in use and what the difference
  costs.

## 6. Milestones

M0, M1 and M2 are done, which is v1. M3 (credential proxy, toolbin, port
forwards, reverse mounts) and M4 (`expose_to` fan-out, `sync` mode) are not
started; `exec_on:` parses and routes but needs M3's reverse mounts to be
useful, and says so at `up` time.
