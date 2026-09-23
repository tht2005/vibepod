# vibepod — design

> Status: **v1 built and verified** (M0-M2) — and **superseded in design by v2**,
> which is what this document now describes. v1 routed commands by working
> directory through a seccomp exec gate; using it showed that the mechanism
> cannot be made trustworthy, so v2 replaces it with an explicit per-session
> **backend** and a live shell on it (§3), adds runtime mounting (§9), makes the
> TUI the primary surface, and solves compute-on-one-machine/data-on-another by
> running a pod on **every** backend so the composed tree is uniform everywhere
> (§6a) rather than translating paths. PLAN.md records what was measured before building
> v1 and the six bugs verification found. §13 has the v2 milestones; §12 the
> remaining unknowns.

## 1. Problem

Working on a remote server over SSH means one of two bad choices:

1. Install the code agents (Claude Code, Codex, OpenCode) on every remote, and copy
   API keys, MCP config, and skills to each one — credentials sprawl across machines
   you may not control.
2. Work locally and sync by hand — losing the remote's toolchain, data, and services.

## 2. Core idea

Invert the direction. Instead of shipping **the agent to the code**, ship **the code to
the agent** and leave **execution where the code lives**.

A *pod* is a local namespace that composes three planes:

| Plane | What it does | Where it lives |
|---|---|---|
| **Composition** | unify local dirs + remote dirs into one filesystem view | **replicates** — the same tree in a pod on every machine (§6a) |
| **Identity** | agent config, skills, MCP, API keys | **never leaves this machine.** Bound into the local pod only |
| **Execution** | run a command on the machine you chose | per-session backend: which pod the shell lives in |

Those three rules are the design, and they compress to one sentence:

> **One namespace, replicated on N machines. The agent lives in exactly one of them.**

Planes 1 and 2 are plumbing. Plane 3 is the product.

## 3. The mechanism

A *backend*, a **live shell** on it per session, and a **dispatching shell** in the pod.

This section replaced an earlier one. v1 routed by working directory through a seccomp
`SECCOMP_RET_USER_NOTIF` gate on `execve`, redirecting with lazy bind-shims. That was
built, it worked, and it was removed. The reason it was removed is the first thing worth
writing down, because it is the only part of this design that was learned by using it.

### Why the exec gate was removed

**A routed `execve` is not a process. It is an `ssh`.** The gate made the call *look*
local, but what ran on the far side was a fresh remote shell, and everything a process
carries beyond `argv`, environment and cwd was gone:

- `/tmp` is a different `/tmp`, so a local writer and a remote reader do not meet
- file descriptors past the first three do not inherit
- no process group, no job control, no `rlimit`s, no cgroup
- signals are best-effort through a pidfile, not delivery to a child
- writes on the far side invalidate the host's FUSE cache non-atomically

No single item is fatal. **The list not terminating is fatal.** You can close any one gap
and still not be able to say "this is correct now", so the mechanism can never be trusted,
so every command gets checked by hand, so it bought nothing. A promise that cannot be
completed is worse than no promise.

The gate was a bad teleporter and an excellent observer — it saw every exec, with its cwd,
before it ran. That is why deleting it has a real cost, recorded under *What this costs*
below. It is not why it was kept.

### What a backend is

A **backend** is a named machine that a session's commands run on: `pod`, or any ssh alias
the pod has mounted. Each session has exactly one current backend, and vibepod holds **one
persistent shell** on that machine for as long as the session lives.

The persistence is the whole point. Most of what made cwd-routing feel broken was that
every command got a new shell: `cd` did not stick, `export` did not stick, background jobs
died, job control did not exist. A session-bound shell fixes that class of problem at once,
and fixes it by *not emulating anything*.

Backend is **per-session state**, never global. The human pressing a key in the TUI and an
agent running a command are separate sessions with separate backends, so neither can move
the other's ground. Two ways to say it, one meaning:

```
vp backend            # what is this session on?          → gpu03
vp use gpu05          # move this session
vp @gpu03 rocm-smi    # one command elsewhere; the mode does not change
```

and `b` in the TUI, which moves the backend of the focused session.

### Interactive: the session *is* the shell

Attach to a session whose backend is `gpu03` and you are in a real shell on gpu03. Not a
proxy, not a per-command `ssh` — the shell vibepod opened for that session, with your
terminal wired to it.

This is why `cd` works: it is `cd`. Shell variables, `jobs`, `fg`, history, `!!`, a
half-typed heredoc — all of it behaves, because none of it is being reproduced. Switching
backend swaps which shell your keystrokes reach; the one you left keeps its cwd and its
jobs, and is still there when you switch back.

Path identity (§5) is what makes this coherent rather than disorienting: `/remote/gpu03/x`
in the pod and `/remote/gpu03/x` on gpu03 are the same string, so moving between backends
does not move the ground under your prompt.

### Non-interactive: the agent's shell stays local

The original argument against intercepting the shell was never wrong, and it is what forces
the split. Measured in this repo, the primary agent runs:

```
$SHELL = /usr/bin/zsh                      # not /bin/sh, not /bin/bash

/usr/bin/zsh -c 'source ~/.claude/shell-snapshots/snapshot-zsh-*.sh 2>/dev/null || true
                 && setopt NO_EXTENDED_GLOB ... 2>/dev/null || true
                 && eval '"'"'<the actual command>'"'"'
                 && pwd -P >| /tmp/claude-XXXX-cwd'
```

Each call is a fresh `zsh -c`. Environment continuity comes from replaying a snapshot file;
cwd continuity comes from writing `pwd -P` to a **local** temp file the next call reads.
Ship that string to a remote and the snapshot path does not exist, `setopt` needs zsh,
`pwd -P >| /tmp/...` writes on the wrong machine so `cd` silently stops persisting, and the
whole thing is zsh syntax handed to whatever shell the remote has.

So a non-interactive shell **always runs in the pod**. The wrapper, the `source`, the
redirections and the `pwd -P` capture all stay where they work. Dispatch happens one level
down, at the program, by two mechanisms that need no kernel help:

- **`/vp/bin` ahead of `PATH`** — a wrapper per remote-only tool (`remote_tools:`) and per
  tool the agent should not run locally. Each is three lines: `exec vp @<backend> <tool>
  "$@"`. This is what `remote_tools:` already meant; it no longer needs a bind-shim to
  stand on.
- **`vp @host cmd` and `vp use`**, which the generated `CLAUDE.md` fragment (§9) teaches.
  This is the honest channel: all three agents read project instructions, which is the
  capability the gate was built to work around.

### The dispatching shell

The pod's `$SHELL` is vibepod's own, roughly sixty lines. It records the command line,
consults the session's backend, and either runs the command in the pod or writes it into
that backend's live shell.

It is bypassable by anything that calls `execve` directly, and **that is the trade**: it
catches what agents and humans actually do — they shell out — at a small fraction of the
complexity of a seccomp gate, and it logs *command lines as written* rather than
post-resolution `execve` argv, which is the more useful record.

### What crosses with a dispatched command

The shell stays local, but a command still has an environment, and which parts of it follow
the command to another machine is a separate question with a different answer.

**Blanket forwarding is refused**, for two reasons that are each disqualifying:

- A pod's environment holds `ANTHROPIC_API_KEY`, `HF_TOKEN` and their relatives. §7
  promises those never leave this machine. Sending the environment sends them.
- `PATH`, `HOME`, `LD_LIBRARY_PATH`, `PYTHONHOME`, `SSH_AUTH_SOCK` and the `XDG_*` family
  describe *this* machine. Imposing them on a remote breaks its toolchain in ways harder to
  diagnose than the thing they were meant to fix — a wrong `PATH` is worse than a missing
  variable.

**Sending nothing is also wrong**, and fails silently, which is worse. A dropped
`PYTHONPATH` surfaces as `ModuleNotFoundError`, which reads as a broken install rather than
a discarded environment, and sends the user to debug the wrong machine.

So what crosses is the **delta**: the difference between the command's environment and the
environment its session started with. That is exactly `VAR=value cmd`, and `export VAR=...`
earlier in the same shell, and nothing else — because anything untouched is by construction
identical to the baseline. Identity variables are excluded even when set deliberately, and
**a refusal is reported rather than swallowed**: it goes to the event stream as a notice,
where `vp log` shows it. Silence is how the original bug survived.

Assignments are emitted before `exec` in the remote script, not after — `exec VAR=v cmd`
would have the shell look for a program named `VAR=v` — which also leaves the pid the
shell's own, so the pid file and the signal path are untouched.

`exec.forward_env` can set this to `none`, or to an explicit list for anyone who would
rather say precisely what travels.

With a persistent per-session shell, the baseline is that shell's own environment at open
time, which makes the delta smaller and better defined than it was under one-ssh-per-exec.

### What this costs

Three things, stated plainly because they were bought deliberately.

- **Bundled binaries bypass `/vp/bin`.** Claude Code's built-in Grep spawns its own
  ripgrep at an absolute path, so it never consults `PATH` and reads through FUSE instead of
  running on the machine that owns the files. The seccomp gate caught exactly this. The
  mitigations are the FUSE cache and telling the agent to dispatch searches; neither is as
  good as catching it.
- **The exec log narrows.** It records what the dispatching shell and `vp` see, which is
  every dispatch and every shelled-out command, but not a direct `execve` from a bundled
  binary. An `rm -rf /remote/gpu03/project` run *locally in the pod* still destroys real
  data on gpu03 through the mount, and now goes unrecorded unless it came through a shell.
- **Pipelines no longer split.** `rg foo | head -20` used to run `rg` remotely and `head`
  locally. Now the pipeline belongs to one shell, so it runs entirely on one backend. This
  is a loss in elegance and a gain in predictability.

### Terminal and signals

SSH does not forward `SIGINT` without a PTY, so Ctrl-C on a dispatched `make` would
otherwise leave an orphan on the remote. But `-tt` merges stderr into stdout and mangles
binary output, and agents parse those streams separately.

So: **piped by default**, with signals forwarded by killing the remote *process group* over
a second multiplexed channel (~5ms, pure POSIX, nothing installed remotely). **PTY only
when the session's own stdio is a tty** — which is exactly an attached session and
interactive programs. This is the same line that separates the two dispatch paths above.

## 4. Architecture

```
┌─ your machine ─────────────────────────────────────────────────┐
│                                                                │
│  vp ──unix socket──► vibepod  (rootless user daemon)        │
│                            ├─ pod registry & lifecycle         │
│                            ├─ route table (path → host:path)   │
│                            ├─ ssh ControlMaster pool           │
│                            ├─ PTY buffers (dtach-style)        │
│                            ├─ credential proxy                 │
│                            └─ audit log                        │
│                            ▲                                   │
│  ┌─ pod "work" (bwrap namespace) │                             │
│  │   vpinit (PID 1) ─────────────┤  holds ns; owns pod mounts  │
│  │   vpsh ($SHELL in the pod) ───┤  logs, then runs or dispatch│
│  │   /vp/bin wrappers ───────────┘  ahead of PATH              │
│  │   /srv/api    ← sshfs  prod:/srv/api                        │
│  │   ~/Git/notes ← bind   (local)                              │
│  │   ~/.claude   ← bind   (local, rw)                          │
│  │   claude / codex / opencode                                 │
│  └────────────────────────────────────────────────────────────┘│
└────────────────────────────────────────────────────────────────┘
         │ ssh, multiplexed — private keys never enter the pod
         ▼
     prod · staging · build-box
```

### Components (one Go binary, five names via argv[0])

| Name | Role |
|---|---|
| `vp` | thin client and the verb surface: `use`, `@host`, `mount`, `hosts`, `log`, `tree` |
| `vibepod` | rootless user daemon, auto-spawned on first use. Owns everything long-lived |
| `vpinit` | PID 1 inside each pod. Holds the mount namespace open, performs every pod mount on its one capable thread — including mounts added long after `up` — reaps zombies, forwards signals |
| `vpsh` | the pod's `$SHELL`. Records the command line, then runs it locally or writes it into the session backend's live shell |
| `vpnode` | `vpinit` on a *backend machine*, pushed on consent: the node's pod. Builds the same composed tree over that node's own system layer, supervises its rclone cache, holds a renewable lease so a job survives a disconnect (§6a) |

`vpinit` exists because **a mount namespace only survives while a process is inside it.**
Without it, detaching would destroy the pod.

## 5. Backends and paths

**The session decides the machine; the path decides the directory.** Two independent
questions, which v1 conflated into one and got wrong in both halves.

| | question | answered by |
|---|---|---|
| **Which machine** | where does this command run? | the session's backend — `vp use`, `b` in the TUI, or `@host` for a single command |
| **Which directory** | what is this path called there? | path identity, then the mount table |

There is no precedence chain any more, and nothing is inferred from the working directory.
A session stays on its backend until something says otherwise, the TUI status bar says
which one, and `vp backend` answers in a word. The old chain — session pin over `exec_on:`
over mount owner over `exec.default` — existed only because the machine was being *guessed*.
`exec.default` survives as the backend a new session opens on. `exec_on:` becomes a
**suggestion** that the TUI and the generated `CLAUDE.md` fragment surface ("this directory
lives on gpu03") rather than a rule that acts on its own — which also disarms the trap where
`exec_on:` could be accepted at `up` and only fail at run time.

Because every backend runs a pod with the same composed tree (§6a), a backend can see the cwd
by construction — the question "does gpu05 have this path" stops being asked at dispatch time.
What is still refused, naming both sides, is a cwd under a mount that node does not yet hold
at the current generation: staleness costs a visible refusal, never a wrong path.

### Multiple hosts

A pod mounts from and executes on **any number of hosts at once**. Nothing in the model is
singular: the route table is a `path → (host, path)` map, the SSH mux pool is keyed by
host, `toolbin` and `forward_credentials` are already per-host, and `vp tree` carries a
host column. A single-host special case would have to be deliberately added, and then
removed again.

Several hosts is where the model earns its keep, because the interesting configurations are
all plural: compute on one machine and data on another (cross-mounts, §6), or code here and
GPUs there. What is genuinely extra is narrow — `expose_to: [a, b]` means one reverse
transport per target, and a command cannot span two hosts, so it picks one side.

`exec_on:` covers "my code is local, the machine that should run it is not". Under §5 it
sets the backend a session **opens on** in that directory, and is shown as a suggestion
rather than applied silently:

```yaml
  - local: ~/Git/proj
    expose_to: [gpu-box]     # gpu-box can see this directory
    exec_on: gpu-box         # a session opening here starts on gpu-box
```

A session that then runs `vp use pod` stays on the pod, and the TUI says so. This is the
part v1 got backwards: a directory that silently changed the executing machine could not be
read off the screen fast enough to be trusted, because agent output scrolls faster than
anyone can follow. The backend is now a thing you set and can see, not a thing a `cd`
decides for you.

Pod-internal paths are permanently exempt, so MCP servers and agent-internal helpers
always run locally and never get shipped to a remote.

### Path identity

A remote directory mounts at **its own absolute path**. `prod:/srv/api` appears in the
pod as `/srv/api`, not `/work/api`.

This is not cosmetic. Two things depend on it:

1. **Arguments forward verbatim.** `cat /srv/api/config.yml` means the same thing on
   both sides. The alternative requires detecting which argv entries are paths and
   rewriting them — heuristic guesswork that fails in rare, confusing ways.
2. **Remote tool output stays openable.** Compiler errors, stack traces, and log lines
   are full of remote absolute paths:

   ```
   error[E0432]: unresolved import
     --> /srv/api/src/db.rs:14:5
   ```

   The agent's next move is to open that file. Under path identity it just works.
   Under a rewritten mountpoint, every path a remote tool prints is one the agent
   cannot open — on every invocation.

**Remote binaries are unaffected by any of this.** A routed command runs on the remote,
in the remote's own real filesystem; the pod namespace exists only on your machine.
Hardcoded paths in remote binaries are as correct as they ever were.

**The hazard runs the other way**: a mount can shadow a *local* system path, so a pod-local
process reading `/usr/lib/...` silently gets remote files over FUSE. `vp up` prevents
this at startup rather than leaving it as a runtime mystery:

- refuse to mount over `/usr`, `/bin`, `/lib`, `/lib64`, `/sbin`, `/etc`, `/proc`,
  `/sys`, `/dev`, or pod runtime dirs
- refuse any target that already exists and is non-empty in the pod
- refuse two mounts claiming the same path

Each refusal names the offending mount and points at `at:`, which overrides placement
explicitly — and re-enables the translation problem for that mount alone.

Real project directories (`/srv`, `/opt`, `/data`, `/var/www`, `/home/deploy`) do not
collide with local system paths, so the guard rarely fires.

## 6. Filesystem

### FUSE cannot be mounted inside the pod

The pod sets `NoNewPrivs=1`, and only one uid is mapped into its user namespace, so
setuid binaries are inert. `fusermount3` is setuid root. Therefore **all mounts are made
on the host by the daemon and bound into the pod.**

Forced, but it yields a security property for free: the agent cannot unmount, remount, or
tamper with any mount. There is no `fusermount -u` available to it.

Getting a mount into an already-running pod is `vpinit`'s job: it is inside the namespace
and is the only process there holding `CAP_SYS_ADMIN`. The daemon sends it a bind request
over the pod socket. `nsenter` from the daemon is *not* an option — joining the userns
grants capabilities, but the `execve` that follows drops them again for a non-root euid.

### Who actually reads through FUSE

Less than it first appears. On a remote backend, `rg`, `make`, and `cargo` execute **on that
machine against its local disk** and never touch FUSE. The FUSE load is only:

- the agent's built-in file tools (Read / Glob / Grep)
- MCP servers
- pod-local commands

The worst of these is the agent's own Grep running ripgrep over the mount. It spawns a
bundled binary at an absolute path, so it consults neither `$SHELL` nor `PATH`, and §3's
removal of the exec gate gave up catching it. **The FUSE performance risk and the
dispatch-gap problem are the same problem wearing two hats** — which is why the VFS cache
carries more weight in v2 than it did in v1, and why the `CLAUDE.md` fragment has to tell
the agent to dispatch its searches.

### Backend: rclone sftp with a VFS cache

| | sshfs 3.7.6 | **rclone sftp + `--vfs-cache-mode full`** | custom Go FUSE |
|---|---|---|---|
| status | mature, maintenance-mode | actively developed | ours |
| second read | round trip | local disk | whatever we build |
| invalidation hook | none clean | `vfs/forget` via rc API | exact |
| effort | none | small | large |

The deciding factor is the invalidation hook — see below. `rclone` is **not** installed on
this machine, so it is a real dependency to vendor or require.

It is also the one component that must run in **two places**. On this machine it composes the
pod's filesystem view; on a compute node it caches another machine's data to local disk and
writes back on close (§6, *Compute on one machine, data on another*). Nothing about
local-disk caching or write-back can be done from here, because the disk being cached to is
not here — which is what makes a remote footprint unavoidable rather than a convenience
(§7).

### Execution-aware cache invalidation

A general-purpose network filesystem must guess: it caches for a second because it has no
idea what is happening on the other end.

**We are not guessing.** vibepod mediates every command, so it knows exactly when the
remote tree could have changed — nothing else touches it. That permits effectively
infinite attribute and entry timeouts, with invalidation driven by **command completion**
rather than a timer. It is a correctness-preserving cache far more aggressive than any
network FS can justify, and it exists only because vibepod knows when a dispatched command
finished. It matters more in v2 than v1: with the exec gate gone, the agent's own bundled
ripgrep reads through FUSE (§3), and the cache is most of what stands between that and a
crawl.

This is why the backend needs a `vfs/forget`-style hook. sshfs has none.

With node pods there is no longer one cache but one per machine holding the mount, so
completion **fans out** to every holder, and the unit narrows from the host to the mount.
§6a, *Many mounts, many caches*, is that part.

### Reverse mounts (`expose_to:`)

Mounts flow both ways. `expose_to: [host]` on a local directory makes it visible **on**
that host, so a command routed there can see local code. Without it, "edit locally, run
on the big machine" is impossible — the target has no such path.

```yaml
  - local: ~/Git/proj
    expose_to: [gpu-box]
```

`exec_on:` is no longer a rule that acts on its own (§5) — it is the backend a session
opens on in that directory, and a *suggestion* the TUI and the `CLAUDE.md` fragment show.
That removes the v1 trap where a config with `exec_on:` and no `expose_to:` was accepted at
`up` and could only fail at run time. What remains true: without a reverse mount, the target
cannot see the path, and **the dispatch is refused with both sides named**.

A reverse mount is **the same problem as the next section with the origin changed**: some
machine must see a directory it does not own, at the path the pod uses. So it uses the same
mechanism — the node's pod carries the path in its composed zone (§6a) and rclone serves the
bytes with a local-disk cache — and the only difference is that the origin is this machine
rather than another remote, which means the served side is the one behind a home uplink.
That asymmetry is why the cache matters more here: the *remote's* reads are the network
reads, and `prefetch:` is usually the right answer for anything larger than source.

### Compute on one machine, data on another

The case that forced this section: gpu05 has the GPUs, gpu03 has the data. Both are mounted
in the pod, so *you* see both — and gpu05 does not see gpu03 at all. Dispatch a command to
gpu05 with a cwd of `/remote/vast0/duongnguyen/proj` and one of two things happens.

The good outcome is that the path does not exist and the command fails.

**The bad outcome is that it does exist and holds different bytes.** Same string, different
filesystem — so `--out ./checkpoints` succeeds, writes somewhere real, and you learn about it
days later. On a cluster where `/remote/...` is a naming convention rather than one shared
volume, this is likely rather than exotic. Path identity is what makes single-machine
dispatch work and is exactly what makes cross-machine dispatch dangerous.

Shared storage is the happy case — CephFS, NFS, Lustre and GPFS homes are the normal state
of a real cluster, and then gpu05 already has the bytes at the path, and the right amount of
vibepod machinery is none. But it cannot be assumed, so the general mechanism has to work
when the mount path and the compute node's own paths have nothing to do with each other.

#### Do not translate paths at run time

The tempting answer is to mount gpu03's data on gpu05 under a private prefix and rewrite
paths in flight. It rewrites `cwd` exactly and absolute paths in `argv` reliably. Then it
misses paths inside a YAML config, paths built at run time in Python, and paths written into
a file for a later job to read.

**That is an unbounded leak list — the precise shape of the thing §3 deleted the exec gate
for.** Introducing a second one would be indefensible. It also breaks the property path
identity was bought for: if gpu05's traceback prints `/tmp/vp-abc/gpu03/remote/vast0/...`,
you cannot paste it into an editor.

#### Reproduce the path instead: every machine runs a pod

vibepod already knows how to build a namespace with arbitrary paths arranged at arbitrary
locations. That is what a pod *is*. So the answer is not to arrange one path per command on
the compute node — it is for **the compute node to run a pod too**, with the same composed
tree. §6a is that design; the rest of this section is what flows through it.

The safety consequence is the reason it is worth the machinery. A node pod's composed paths
are ones vibepod placed, so gpu05's *own* `/remote/vast0/...` is not in the namespace at all
unless we put it there. The same-string-different-bytes catastrophe stops being something to
detect and becomes something that cannot be expressed.

#### Writes: cached on local disk, written back on close

A checkpoint written over a network filesystem stalls the step loop, and a timer that sweeps
for changes is bloat. Both are avoided by the mount already being there:
`--vfs-cache-mode full --vfs-write-back` puts writes on gpu05's own NVMe immediately and
uploads after last use. The trigger is **file close, not a clock**.

Reads cache to the same local disk, which is why this one mechanism ends up doing three jobs
that would otherwise be three features:

| want | mechanism |
|---|---|
| a dataset on fast local disk | the read cache, warmed as the job reads |
| checkpoints that do not stall training | write-back on close |
| re-reads that do not hit the network | the same read cache |

This is also why mounts need no `role:` — there is no project/dataset/output distinction
left to declare, only `readonly:` and the cache's own bounds.

The one case the lazy cache handles badly is many small files: a first epoch over 1.28M
JPEGs is latency-bound while the GPUs idle, even though every epoch after is local NVMe. So
`prefetch: true` on a mount does an upfront `rclone copy` for exactly that shape. Opt-in,
because a run that touches one percent of a tree should not pay for all of it.

#### Where the bytes come from

Independent of how the path is arranged: gpu05's rclone reaches gpu03 either **directly**
(one hop, full speed, authenticating through your forwarded agent socket so no key is ever
stored there) or **relayed through this machine** (two hops, works with no node-to-node
connectivity at all). Node-to-node ssh is firewalled on many clusters and
`AllowAgentForwarding no` is common, so `via:` defaults to `auto`, probes at mount time, and
**reports what it chose and why the alternatives were ruled out**:

```
gpu05 → gpu03  direct: connection refused (node-to-node ssh filtered)
               relaying through this machine (2 hops, ~31ms + ~12ms)
```

rather than timing out with nothing to go on.

### Mount modes

`mode:` is per-mount, so the strategy can change without changing the config shape.

| mode | behaviour | good for |
|---|---|---|
| `fuse` (default) | rclone sftp + VFS cache, invalidated on command completion | most repos |
| `sync` | bidirectional sync to a local scratch dir | huge repos, very high RTT |
| `bind` | local directory, no network | local dirs |

### Failure modes

Link drop with open fds yields stale handles and `EIO`; the daemon remounts and reports it
(§7a). Buffered writes lost to a drop can leave a partial file. Log directories and other
read-only sources should be mounted `readonly: true`.

## 6a. The replicated tree

**Every backend runs a pod.** Your machine runs one, gpu03 runs one, gpu05 runs one, and all
of them present the same composed filesystem. A *backend* (§3) is therefore which pod your
session's shell lives in, and the model is one sentence:

> One namespace, replicated on N machines. The agent lives in exactly one of them.

This is smaller than what it replaces, and it is mostly existing code aimed at another
machine: `buildRoot`, `pivot_root` and `vpinit` already do this once. `vpnode` is `vpinit`
with the identity plane omitted.

### What replicates, and what must not

§2's three planes turn out to have different replication rules, which is the whole design:

| plane | rule |
|---|---|
| **Composition** | replicates — identical on every node, in config order |
| **Identity** | **never leaves this machine.** Credentials, agent config, MCP, keys. Node pods are filesystem-uniform and identity-free |
| **Execution** | per-session: which pod the shell is in |

Within composition, uniformity is scoped, and the scope matters:

- **The composed zone** — every mount named in config. Identical on every node, same paths,
  same order. This is the guarantee.
- **The system layer** — `/usr`, `/lib`, `/opt`, `/etc`, `/dev`. The node's **own**, and
  necessarily different, because that difference is the reason to dispatch at all. gpu05's
  `/opt/rocm`, `/dev/kfd` and its driver libraries are precisely what this machine does not
  have. `remote_tools: [rocm-smi, rocminfo, hipcc]` exists because of that difference.

Making the system layer uniform would mean installing a userspace on every server and losing
the node's own toolchain — the thing §1 exists to avoid. So it stays per-node, and a node pod
is built as **the node's own system layer with the composed zone applied over it**, not as a
minimal root. On this machine the pod is minimal because it contains an agent and containment
is the point; on a node there is no agent to contain and a toolchain to preserve. The
asymmetry is deliberate.

What can be checked instead of imposed: a mount may declare what the machine running it needs.

```yaml
  - remote: gpu03:/remote/vast0/duongnguyen/proj
    requires: [/opt/rocm, /dev/kfd]    # a node lacking these is refused at `up`
```

That converts "is this node actually equivalent?" from a thing you find out when a job fails
into a thing `up` answers.

### Order is part of the state

Mount order decides shadowing: with mounts at `/a` and `/a/b`, order decides what is visible.
So the desired state is an ordered **list**, not a set, and every node applies it in the same
order.

Inserting into the middle of that list on a live node would mean unmounting and remounting
everything above the insertion point — churn at exactly the moment something is running in
those paths. So **runtime mounts append**. Order is insertion order: identical everywhere,
reproducible from the file, and undisturbed by removals. Reordering is a config-time
operation that needs `up`, and `vp save` writes the resulting order back.

A runtime mount that would shadow an existing one is **refused, with both paths named** —
the same guard the systemPaths deny-list already applies at `up`.

### Keeping the tree the same: reconcile, do not transact

The composed tree mutates at runtime (§9), so it is replicated mutable state. That makes this
a small control plane, and the shape is a reconciler:

- Desired state is the ordered mount list, each entry with a monotonic **generation**.
- Each node pod reports the `(mount, generation)` pairs it currently holds.
- A change bumps the generation and pushes; each node converges independently; a node that
  was unreachable applies the diff when it returns.
- **Dispatch is the guard.** A command whose cwd resolves into a mount the target node does
  not hold at the current generation is refused, naming both sides. Staleness therefore
  cannot cause a wrong-path execution — it can only cause a visible refusal.

Two-phase commit is the obvious alternative and is rejected: one unreachable node would block
every mount change, and it blocks on participants that may never return. Best-effort push
with no tracking is rejected for the opposite reason — it produces exactly the silent
divergence this section exists to prevent.

**Staleness is tracked per mount, not per node.** If gpu08 missed a new mount, commands there
under *other* mounts are still correct and keep working; refusal is scoped to the paths
actually affected. It is a map lookup, not extra machinery, and it is the difference between
one missed mount costing a path and costing a machine. `vp ps` and the TUI show which nodes
are behind and on what.

### Many mounts, many caches

Every pod holds the whole composed zone, so a backend does not have "its" mounts — it has
all of them. Several mounts from one host is likewise ordinary: `HostList()` dedupes aliases
and one ssh mux serves every mount from that host.

What follows from replication is that **one mount exists as N mounts**, and their
relationship needs stating.

#### Native on its owner, cached everywhere else

gpu03's pod binds gpu03's own directories directly — no FUSE, no cache, no round trip — and
only rclone-mounts the ones owned by other machines. So running work on the machine that owns
the data is always full speed, with no special case and nothing to configure. It also means a
node pod is typically a *mixture*: some entries in its composed zone are native binds, others
are cached mounts, and `vp tree` shows which.

#### Writes: one writer per file

Write-back caching (§6) puts dirty data on the writing node's own disk. With N caches over one
mount, two nodes can each hold dirty data for the same file, and whichever flushes last wins —
**silently**. That is the same class of failure as the same-name collision §6a exists to
eliminate, so it gets the same treatment rather than a caveat.

The control plane tracks **open-for-write per `(mount, path)`**. A second node opening that
file for writing is refused, naming the holder:

```
open /remote/vast0/duongnguyen/runs/ckpt-400.pt: gpu05 holds this file open for writing
```

Per *file*, not per mount, because that is the unit rclone already works in — write-back
triggers on close — and because two nodes writing different files in one output mount is
normal, while two writing the same file is the error it appears to be. The token is released
when the file goes clean, and a node that dies holding one releases it with its lease (§6a).

#### Reads: invalidate by mount, to every holder

§6's invalidation assumed one cache. It now fans out: when a dispatched command completes, the
control plane tells **every node holding the mounts that command touched** to forget them.
M6a's reconciler already carries this traffic, so it is a message type rather than a
mechanism.

This also narrows invalidation from per-host to **per-mount**. `InvalidateAfter(host)` forgets
every mount belonging to a host, which was harmless with one cache and one or two mounts, and
is needlessly destructive once a host owns several and each has N copies.

#### Overlapping origins are refused

Mounting `gpu03:/data` and `gpu03:/data/sub` separately produces two caches over the same
bytes, where a write through one is invisible to the other. There is no way to fix that after
the fact, so it is refused at `up` and at `vp mount`, naming both mounts.

This extends the existing shadow guard, which checks only that a mount does not shadow a
`systemPath` — a check on *pod* paths. Origin paths need the same check for a different
reason: not visibility, but coherence.

#### Connection budget

M mounts × N node pods all pull from one origin. gpu03 serving three mounts to three node pods
is around nine sftp sessions plus control channels, against sshd's defaults of `MaxSessions
10` and `MaxStartups 10:30:100`. Exceeding them on a shared login node is antisocial and
presents as unrelated connection failures for everyone.

So each node pod runs **one `rclone rcd`** with bounded per-host connections, and `up`
**computes and reports the budget** rather than letting it be discovered:

```
gpu03: 3 mounts × 3 nodes → 6 sftp connections (cap 8, sshd MaxSessions 10)
```

### Disconnection

Three different failures, deliberately handled differently.

**Deliberate unmount.** Write-back caching (§6) means a node may hold dirty data on its local
disk, so unmount is two-phase: **flush, verify clean, then unmount** everywhere, then drop
from desired state. If the flush cannot complete because the origin is unreachable, the
unmount is **refused and names what is dirty**. Silently discarding a checkpoint would be the
worst bug this system could have.

**A link drop to a node.** There is a real tension here and no way to dodge it: dying with
the ssh channel kills an eight-hour training run when a laptop lid closes, and surviving
forever leaks namespaces, rclone processes and caches onto someone else's shared node.

A **lease** resolves it. `vpnode` renews while connected and survives disconnection for the
lease duration; on expiry it flushes what it can, unmounts, and exits. Reconnect reattaches
with output replayed from a ring buffer on the node — which is §10's detach/reattach design
running one level out, the same as everything else in this section.

**This machine dying.** Node pods outlive the daemon under the lease, so the next `up` of the
same pod **adopts** them: find the lease handle on the node, reattach, do not start fresh. A
training run survives a reboot, which for the use case driving this design matters more than
almost anything else here. Adoption checks the build, because a mismatched `vpnode` is the
stale-daemon bug already paid for once.

### What this deletes

Worth recording, because the design got smaller:

- **The uniform-prefix fallback.** A node pod can place `/remote/vast0/...` wherever it likes,
  so nothing forces a `~/.vp/<pod>/...` compromise and the remote's own absolute paths never
  stop resolving.
- **`compute_on:`** — when every node has the tree, there is no per-mount declaration of who
  needs to see what. It survives only as an optional prefetch hint.
- **The marker file as a safety guard.** It stays as a cheap sanity check that rclone attached
  where it was told, but the catastrophe it defended against is now inexpressible.

### What it costs

- **Unprivileged user namespaces become mandatory on every backend**, with no fallback: no
  pod means no path guarantee and no collision safety, so the machine cannot be a backend.
  `doctor` names the sysctl and what to ask an administrator for. §12's question about how
  often this is disabled stops being academic.
- **This is a distributed system now.** Generations, reconciliation, leases and adoption are
  a control plane, and the failure modes are the ones control planes have. §13 sequences it
  so a single-node pod works before any of it is built.
- **Mount count × machines.** One `rclone rcd` per node pod manages many mounts in one
  process, and mounts materialise on first use rather than at `up` — the tree is a guarantee,
  not an eager allocation. That rc API is the same one §6 already wanted for `vfs/forget`.
- **First-use latency** per node: a second or two cold (ssh, push binaries, rclone,
  namespace), a few hundred milliseconds warm. Per backend, not per command.

## 7. Security model

**What never leaves your machine**

- API keys and agent credentials. Bound into the pod, never transmitted to a remote.
- SSH private keys. **The pod cannot see them at all** — `vibepod` runs outside the pod
  and owns every SSH connection. A misbehaving agent has no key material to exfiltrate.

**What does cross the wire**

- Command strings, to the routed host.
- Remote source code, to your machine (via FUSE) and to the model API. Inherent to the
  premise; stated plainly.

**Reverse credential proxy.** A command routed to `prod` running `git push` needs
credentials `prod` doesn't have. `vibepod` exposes your local ssh-agent and git
credential helper back over the existing channel, scoped to that command's lifetime.
Nothing is stored remotely — it is proxied, not copied. Honest caveat: while it runs,
root on that host can use your agent. Hence `forward_credentials` is per-host config.

**Remote footprint.** Nothing is installed speculatively, and nothing system-wide. Two
kinds of push, both user-owned under `~/.vp/bin` and both removable with `vp clean @host`:

- **Missing tools** (`rg`, `fd`, `jq`) that a dispatched command needs. Prompted; `toolbin:
  true` pre-authorizes a host so agent runs are not interrupted.
- **`vpnode` and `rclone`** on a machine used as a *compute* backend for another machine's
  data. Asked once per host, then remembered.

The second is a genuine change from "nothing but ssh on the remote", so it is worth being
precise about why it is not optional. Caching a dataset on gpu05's NVMe and writing
checkpoints back on close require a process **on gpu05**; no amount of cleverness here can
write to a disk over there. The alternative is not a smaller footprint, it is not having the
feature. What the rule still guarantees is unchanged and is the part that mattered: no
credentials, no agent install, no key material, nothing outside a directory you own.

**Audit log.** Every command and the machine it landed on, in one place. For a tool whose
pitch is "your agent runs commands on prod", provable history is a requirement.

**Host access is an explicit allowlist.** Only paths named in `host_access:` are bound
into the pod. An agent working on a client's remote code cannot read `~/Documents`,
`~/.aws`, or a sibling client's repository. Nothing is granted implicitly.

**Honest limit.** bwrap is a namespace, not a security boundary against a determined
attacker. It contains accidents, not adversaries.

## 7a. Failure behaviour

When an SSH link drops mid-command, the command **fails loudly** with a reserved exit
code (`75`, `EX_TEMPFAIL`) and a clearly-vibepod error on stderr. The daemon reconnects
in the background so the next command succeeds.

It does not silently retry. A dispatched `make deploy` or migration that already partially
executed must not be re-run behind the agent's back — a visible failure the agent can
reason about is strictly better than a half-applied change it never learns about.

A dropped link to a **backend** is a different event, because the node pod and whatever it is
running are still alive over there. That is handled by the lease in §6a — survive the blip,
reattach with replayed output, and tear down only when the lease expires — so a long job is
not collateral damage of a closed laptop lid.

## 8. Config

Two layers: per-project `vibepod.yaml`, global `~/.config/vibepod/config.yaml`.
Hosts are **ssh_config aliases** — ProxyJump, keys, ports, and forwarding are inherited,
never reimplemented.

```yaml
pod: work
agents: [claude, codex]

hosts:
  prod:
    toolbin: true
    forward_credentials: false
  gpu-box:
    forward_credentials: true

mounts:
  - remote: prod:/srv/api          # → /srv/api in pod; a session opening here starts on prod
    mode: fuse
  - local: ~/Git/proj              # → ~/Git/proj in pod
    expose_to: [gpu-box]           # ...and visible on gpu-box
    exec_on: gpu-box               # ...where its commands run
  - local: ~/Git/notes             # plain local dir, commands run in the pod
  - remote: prod:/var/log/api
    mode: fuse
    readonly: true
  - remote: staging:/srv/api       # collides with prod:/srv/api
    at: /staging-api               # explicit override required
  - remote: gpu03:/remote/vast0/duongnguyen/imagenet
    readonly: true
    requires: [/opt/rocm, /dev/kfd]  # a backend lacking these is refused at `up` (§6a)
    via: auto                        # direct | relay; auto probes and reports
    cache: 200G                      # on each node's own local disk
    prefetch: true                   # many small files: copy up front, don't warm lazily

remote_tools:                      # exist only on a remote; shimmed in /vp/bin
  - rocm-smi                       # ahead of PATH, since nothing here to shadow

host_access:                       # bound from host into pod
  - ~/.claude
  - ~/.config/opencode

ports:
  - prod:3000                      # auto -L forward

can_mount:                         # what `vp mount` may reach from inside the pod
  - "gpu*"                         # default: any host in your ssh config
  - lab-7

lease: 24h                         # how long a node pod outlives a lost connection
                                   # before it flushes, unmounts and exits (§6a)

exec:
  default: pod                     # the backend a new session opens on
  forward_env: delta               # what the caller set, only (§3); or none,
                                   # or an explicit list of names
```

Split `vibepod.yaml` (committed, shareable) from `vibepod.local.yaml` (your paths,
gitignored).

Every field here is also settable while the pod runs (§9), and `vp save` writes the live
state back. The file is a starting point and a snapshot, not a thing you restart for.

## 9. Interface

### Command surface

Two names. Bare `vibepod` opens the TUI; `vp` is the verb surface that you and the agent
both type all day. The v1 names `vpctl` and the `vpsh` *shim* are retired — `vpsh` is
reused as the pod's `$SHELL` (§3), which is never typed.

```
vibepod [pod]                 open the cockpit                     (the TUI, below)
vibepod up [name]             create pod, detached, no TUI         (scripts, CI)
vibepod down [pod]            stop, unmount, disconnect
vibepod doctor                namespaces, mounts, ssh reachability, toolchains

vp use <host|pod>             move this session's backend
vp backend                    which backend is this session on
vp @host cmd ...              one command elsewhere; the mode does not change
vp hosts                      machines this pod knows, mounted or not
vp mount <host>:<path> [at]   connect and mount into a running pod
vp unmount @host              unmount and disconnect
vp cd @host                   go to a machine's directory
vp where [@host]              what this directory is called on a machine, or the reverse
vp shell [pod]                another independent terminal into a running pod
vp attach [pod]               reattach to a detached session
vp ps                         pods, backends, health
vp tree [pod]                 mounts and live session/exec structure
vp log [-f] [pod]             every dispatched and shelled command, and where it ran
vp save                       write live state back to vibepod.yaml
```

Pods are **named** and globally listable, but a bare `vibepod`/`vibepod up` in a project
directory takes its name from `./vibepod.yaml`.

#### The config is a live object, not a boot artifact

`vp mount`, `vp unmount` and `vp save` answer the complaint that shaped this revision:
adding a machine meant `down`, edit YAML, `up` — losing every session and the agent's
context with them. **Anything in `vibepod.yaml` is changeable while the pod runs**, and
`vibepod.yaml` becomes a snapshot you can *take* rather than a file you restart for.

The mechanism was already there and unused: `vpinit`'s mount worker is a thread pinned with
`CAP_SYS_ADMIN` for the pod's whole life, `OpBind` is a generic `Src`→`Dst`, the FUSE
manager's `Add` is incremental, and the ssh pool connects lazily. The only frozen piece was
the route table, which becomes swappable under a lock.

Mounting is outward-facing in a way `rm -rf` is not: it opens a network path from inside a
sandbox whose purpose was containment. So unlike the guardrail ruling below, it is **not**
unrestricted — any host already in your ssh config is allowed, anything else is refused, and
when a TUI is attached an agent's mount request surfaces as a one-key confirmation.

### The TUI

Bare `vibepod` opens a cockpit: machines, sessions, and the live log, keyboard-driven.

```
┌ vibepod · demo ─────────────────────────────────────────────┐
│ MACHINES                   │ ACTIVITY                       │
│ ● pod         local        │ 14:02:11 gpu03 python train.py │
│ ● gpu03  12ms /remote/gpu03│          … running 4m12s       │
│ ● gpu05  31ms /remote/gpu05│ 14:01:40 pod   rg TODO    ✓ .3s│
│ ○ lab-7   —   unmounted    │ 13:58:02 gpu05 ls -l      ✓    │
│                            │                                │
│ SESSIONS                   │                                │
│ 1 claude     pod     4m    │                                │
│ 2 shell    ▸ gpu03  12m    │                                │
├─────────────────────────────────────────────────────────────┤
│ backend gpu03 │ m mount  u unmount  b backend  ⏎ attach  q │
└─────────────────────────────────────────────────────────────┘
```

`m` mounts (prompting for `host:path`), `u` unmounts, `b` moves the focused session's
backend, `⏎` attaches, `/` filters the log, `q` quits.

#### Handoff, not nesting

`⏎` does not render a terminal inside a pane. It **leaves the alt-screen, restores the
terminal to exactly what the program expects, attaches raw, and redraws on detach** — what
`lazygit` does with `$EDITOR`.

This is deliberate, and it is the reason the TUI is a cockpit rather than a multiplexer.
Running Claude Code inside a homemade multiplexer means nested alt-screens, mouse reporting
fighting mouse reporting, bracketed paste arriving mangled, and resize storms — a large
budget spent reimplementing tmux badly, and a *worse* `claude` at the end of it. Handoff
costs a tenth of the work, has none of those failure modes, and gives up only side-by-side
panes, which tmux already provides for anyone who wants them.

The detach key therefore belongs to the **session**, not the TUI: swallowing keystrokes that
Claude Code wants is exactly the failure being avoided. `ctrl-\` is unclaimed by all three
agents.

A pod supports **many concurrent sessions** — `vp shell` attaches another independent
terminal to a running pod, `docker exec -it` style. Each session has its own cwd and its
own backend.

### The tree

`vp tree` is the one view nothing else can produce. `pstree` stops at the machine
boundary; vibepod knows every session's backend and every command it dispatched, so here
**the machine is a column**. It renders the pod's whole structure — what is mounted from where,
and what is executing where — and replaces a separate `vp mounts`.

```
$ vp tree work
work · running 3m12s · 2 sessions

mounts
├─ /srv/api         ← prod:/srv/api       fuse  rw   8ms  ✓
├─ /var/log/api     ← prod:/var/log/api   fuse  ro   8ms  ✓
├─ ~/Git/proj       ← local               bind  rw        ✓
│  ├─ exposed to    → gpu-box             sftp-R    14ms  ✓
│  └─ exec_on       → gpu-box
├─ ~/Git/notes      ← local               bind  rw        ✓
└─ ~/.claude        ← local host_access   bind  rw        ✓

exec
├─ session 1  (console)
│  └─ claude                              pod      3m12s
│     ├─ (47 completed)                            1m02s
│     └─ cargo test                       prod     12.4s  ●
└─ session 2  (vp shell)
   └─ zsh                                 pod      8m40s
      └─ python train.py                  gpu-box  1m04s  ●
```

The two halves are deliberately in one view: **the mount tree explains the exec tree.**
"Why did that run on gpu-box" is answered four lines up, by the `exec_on` under
`~/Git/proj`.

Sessions are the roots, since a pod has several. Completed subtrees collapse to a count —
an hour of agent work is hundreds of execs, and an uncollapsed tree is unreadable.

**Remote depth.** We know what we dispatched, not what it spawned, so a dispatched command
is a **leaf**:
the `rustc` and `ld` that `cargo` spawns on prod are invisible. `vp tree -x` polls
`ps --ppid` over the warm mux to expand a remote subtree on demand, marked as polled and
approximate. Honest by default, deep when asked.

**One data source, three renderings.** The daemon emits a single event stream (exec start,
exec exit); the TUI's activity pane, `vp tree -f`, and `vp log -f` are all subscribers.
Collection is solved by the dispatching shell — this is only a rendering problem.

```
vp tree --json                 # frugal: live in full, completed as counts
vp tree --json --all           # everything the daemon knows
vp tree --json -f              # NDJSON, one event per line
vp tree --running              # what is still alive
vp tree --failed --since 10m   # what broke recently
vp tree 412                    # one subtree
vp tree --mounts | --exec      # one half
```

**Agent-facing output is frugal by default.** The human view collapses completed subtrees
for readability; `--json` collapses them for context budget — an hour of agent work is
hundreds of execs, and a complete tree is tens of kilobytes spent on `rustc` invocations
nobody asked about. `--all` is there when something is genuinely parsing it.

Note that plain `vp tree` is already a fine agent interface, and a cheaper one — JSON
costs roughly twice the tokens for the same facts:

```
{"pid":412,"argv":["cargo","test"],"target":"prod","state":"running","elapsed_ms":12400}
412  cargo test  prod  running  12.4s
```

`--json` is for consumers that parse (a script, `jq`), not for consumers that read. Which
is a reason to keep the default rendering compact and column-aligned rather than pretty.

Streaming is **NDJSON events**, the daemon's own event stream exposed directly — one
object per line, tailable, composable:

```
{"v":1,"ev":"exec","pid":412,"ppid":88,"argv":["cargo","test"],"target":"prod","ts":"…"}
{"v":1,"ev":"exit","pid":412,"code":0,"elapsed_ms":12400,"ts":"…"}
```

`"v": 1` matters more than it looks: the moment an agent parses this it is an API, and it
will outlive several rounds of the tree's visual layout.

`vp log` and `vp tree` pair rather than overlap: **log is flat, chronological,
finished; tree is hierarchical, live, running.**

### The in-pod control plane

The agent runs *inside* the pod, where `vp` reaches the daemon through a socket bound
into the namespace. That socket is a control plane, and reading is not the same as writing.

| | pod socket | host socket |
|---|---|---|
| `tree`, `log`, `ps`, `where`, `hosts`, `backend` | ✓ | ✓ |
| `use` (own session only) | ✓ | ✓ |
| `mount`, `unmount` | ✓ *allowlisted hosts only* | ✓ |
| `down`, `save`, `up` | ✗ | ✓ |

v1 put a tty check on `use`, reasoning that an agent must not re-route itself. **v2 drops
that**, because in v2 an agent choosing its own backend is the entire interface — it is how
work reaches gpu03 at all. What was a privilege is now the mechanism, and the thing it was
protecting (you not knowing where a command ran) is handled instead by the backend being
explicit, per-session, and on screen.

`mount` is where the real boundary moved, and it is a different kind of boundary: it opens a
**network path** out of a sandbox built for containment, which no `rm -rf` does. So hosts
already in your ssh config are allowed, anything else is refused, and with a TUI attached the
request surfaces as a one-key confirmation. That is a proxy for intent, not proof of it. It is
cheap, it fails closed, and the alternative — an explicit grant step — costs a round trip at
exactly the moment you
are trying to work.

### Knowing where things ran

Two audiences, two mechanisms.

**You:** `vp log -f` is the trust surface. Since the daemon mediates every exec, it can
show the one thing nothing else can — what the agent is doing *and where*. It is a primary
surface, not a debugging afterthought.

**The agent:** vibepod writes a `CLAUDE.md` fragment into the pod, in the agent's own
language. No MCP server, no new tool to learn.

```
You are in a vibepod. Commands run on the machine that owns their directory:
  /srv/api      → prod     (remote)
  ~/Git/proj    → gpu-box  (local files, remote execution)
  ~/Git/notes   → local

Run `vp tree --json` to see what is executing and where.
```

### Prompts

Setup also *reports* itself at `up` time. Connecting to a machine that turns out
not to exist costs ten seconds, and a wait that says nothing is
indistinguishable from a hang:

```
vibepod: connecting to gpu03… connected (240ms)
vibepod: mounting gpu03:/srv/api via sshfs… mounted (310ms)
```

A failure names the step and explains itself in terms of the next action —
which key to add, which `Host` entry to check — rather than forwarding ssh's
own diagnostics, which are written for someone debugging ssh. The step line says
*which*; the error says *why*; neither repeats the other.

Everything vibepod needs to ask — toolbin pushes, credential forwarding, host trust — is
asked at **`up` time**, while a human is certainly watching. Mid-run, a question only
appears if a terminal is attached; otherwise the command fails fast and actionably:

```
vibepod: rg not available on prod
         run `vp allow toolbin prod` to push it
```

### Guardrails

There are none, by choice. vibepod routes and records; it does not judge. The agent's own
permission system already gates commands, and pattern-matching shell strings for `rm -rf`
is leaky in both directions — false positives block real work, and evasion is trivial.
The log is the answer.

Non-invasive by default: `VIBEPOD_POD`, `VIBEPOD_SESSION` and `VIBEPOD_BACKEND` are
exported and a prompt snippet is opt-in, rather than rewriting anyone's `PS1`.

## 10. Detach & reattach

`vibepod` owns the PTY and keeps a scrollback ring buffer (dtach/abduco-style, built in —
no tmux dependency). Detaching leaves the agent running; `vp attach` reconnects and
replays the buffer. Killing the pod kills everything inside it.

## 11. Decisions locked

| Decision | Choice | Why |
|---|---|---|
| Pod backend | own user+mount+pid namespace | ~10ms start, no image, reuses host binaries. bubblewrap cannot host this design — see below |
| Topology | central `vibepod` daemon + per-pod `vpinit` | one socket, one audit log, shared ssh muxes; `vpinit` covers the namespace-lifetime requirement |
| Privilege | rootless, auto-spawned | credentials are user-owned; root buys nothing and costs the security story |
| Exec target | per-session backend; `@host` for one command | the machine is chosen, never guessed. cwd-inference was built, used, and removed — §3 |
| Dispatch | the pod's `$SHELL`, plus `/vp/bin` wrappers ahead of `PATH` | bypassable by a direct `execve`, and worth it: it catches what agents and humans actually do at a fraction of a seccomp gate's cost, and logs command lines rather than resolved argv |
| Remote execution | one live shell per session per backend | `cd`, `export`, jobs and job control stop needing emulation, because nothing is emulated. One-`ssh`-per-exec is what made routing untrustworthy |
| Sandbox | native `clone` + `pivot_root`, behind an interface | bwrap nests a *second* userns after building the root, so nothing inside can mount — and mounting after `up` is required for `vp mount`. Measured, not assumed |
| Capabilities | `CAP_SYS_ADMIN` to `vpinit` via the ambient set, ambient then cleared | vpinit needs it for the pod's whole life; nothing it spawns gets any |
| FUSE placement | mounted on the host, bound in | `NoNewPrivs=1` kills setuid `fusermount3`; the agent also cannot tamper with mounts |
| FS backend | rclone sftp + VFS cache | local-disk re-reads, and a `vfs/forget` hook for execution-aware invalidation |
| TTY & signals | piped by default, PTY when stdio is a tty | agents parse stdout/stderr separately; signals forwarded by remote process-group kill |
| Remote FS | `fuse` default, `mode:` per mount | ship fast, escape hatch for latency without a config break |
| Detach | dtach-style, built into the daemon | no tmux dependency, no prefix-key collisions |
| Credentials | per-command reverse proxy, per-host opt-in | nothing stored remotely; trust decided per machine |
| Remote footprint | push on demand, prompt first, `toolbin:` pre-authorizes | no speculative installs, no mid-run interruptions once trusted |
| Pod identity | named, resolved from cwd's `vibepod.yaml` | docker-like when explicit, zero-argument in a project |
| Path identity | mount at the remote's own absolute path | args forward verbatim; remote tool output stays openable. Shadowing guarded by a deny-list at `up` |
| Remote-only tools | `remote_tools:`, three-line wrappers in `/vp/bin` ahead of `PATH` | the GPU box's tools are not on your laptop, and with no exec gate a wrapper no longer needs a binary to shadow |
| Host access | explicit allowlist | nothing granted implicitly; the agent cannot read unrelated projects or credentials |
| Link drops | fail loudly, exit `75` | never silently re-run a partially-applied non-idempotent command |
| TUI | cockpit plus terminal handoff | dashboard for machines, sessions and the log; selecting a session suspends the TUI and hands the raw terminal over, so Claude Code is never nested inside our alt-screen, mouse reporting or bracketed paste |
| Navigation | by machine (`vp cd @host`), never plain `cd` | path identity makes paths long; a directory can be named `@host`, and a `cd` that guessed would silently change machines |
| Prompt | full path plus the machine it runs on | the path is the honest cost of path identity; the machine is what you need before pressing return |
| Sessions | many per pod | `vp shell` attaches independent terminals, `docker exec -it` style |
| Live config | every `vibepod.yaml` field changeable at runtime; `vp save` snapshots | a config that needs a restart costs you the session and the agent's context to add one machine |
| Reverse mounts | `expose_to:`, sharing the compute-node mechanism | "edit locally, run on the big machine" is impossible without them, and it is the same path-reproduction problem with the origin changed — not a second mechanism |
| Visibility | live exec log + generated `CLAUDE.md` | one mechanism per audience; no output annotation to corrupt parsed streams. The fragment carries more weight in v2: it is how the agent learns `vp` at all |
| `vp tree` | mounts and exec structure in one view | the mount half explains the exec half; replaces a separate `mounts` command |
| Remote depth | leaf by default, `-x` polls `ps` | we see what we dispatched, not what it spawned; do not fake fidelity we lack |
| In-pod scope | read-only, plus own-session `use` and `mount` | the agent moving *its own* backend is the design, not an escape; what it may mount is allowlisted instead |
| Routed env | forward the caller's delta; never identity, never credentials | blanket forwarding breaks §7's promise and the remote's toolchain; sending nothing fails silently as a broken install |
| Guardrails | none — the log is the answer | pattern-matching shell strings is leaky both ways; the agent already gates commands |
| Hosts | ssh_config aliases | inherits ProxyJump/keys/ports for free |
| Cross-machine paths | **every backend runs a pod** with the same composed tree (§6a) | translating paths at run time misses config files, run-time-built paths and paths handed to later jobs — the same unbounded leak list §3 deleted the gate for. A node pod places the path instead, so a node's own same-named directory is not in the namespace and the catastrophe is inexpressible rather than detected |
| Uniformity scope | composed zone identical everywhere; system layer is the node's own | the system layer *must* differ — gpu05's ROCm and `/dev/kfd` are why you dispatch there. Uniform userspace would mean installing one per server, which §1 exists to avoid. `requires:` checks equivalence instead of imposing it |
| Node root | the node's own system layer, composed zone over it | the local pod is minimal because it holds an agent; a node holds a toolchain and no agent. A minimal root on a GPU box hides the GPUs, and that failure reads as "ROCm is broken" |
| Tree consistency | per-mount generations, reconcile on reconnect | 2PC lets one dead node block every mount change; best-effort push produces silent divergence. Dispatch refuses a stale mount, so staleness costs a visible refusal, never a wrong path |
| Staleness grain | per mount, not per node | one missed mount should cost a path, not a machine; unrelated sessions on that node stay correct |
| Mount locality | native bind on its owner, cached rclone mount elsewhere | running work where the data lives is then always full speed, with nothing to configure |
| Write coherence | one writer per **file**, tracked by the control plane | N write-back caches over one mount means two nodes can hold dirty copies and the last flush wins silently. Per-file because that is rclone's unit and because two nodes writing *different* files in one mount is normal |
| Invalidation grain | per mount, broadcast to every holder | one cache became N; forgetting a whole host's mounts was harmless with one copy and wasteful with many |
| Overlapping origins | refused at `up` and `vp mount` | two caches over the same bytes cannot be made coherent afterwards. The existing shadow guard checks pod paths for visibility; origins need the same check for coherence |
| Connection budget | one `rclone rcd` per node, budget computed at `up` | M mounts × N nodes against one origin runs into sshd's `MaxSessions`/`MaxStartups`, which presents as unrelated failures for everyone on a shared node |
| Runtime mount order | append-only; reorder needs `up` | order decides shadowing, so it is part of the state. Inserting mid-list would remount everything above it while work is running in those paths |
| Backend disconnect | renewable lease, then flush + unmount; adopt on reconnect | dying with the ssh channel kills an 8-hour run on a closed lid; surviving forever leaks caches onto shared nodes. A lease is the only answer that does neither |
| Unmount | flush, verify clean, then unmount — refuse if dirty and unflushable | write-back caching means a node holds dirty checkpoints; discarding one silently would be the worst bug here |
| No userns on a node | refuse it as a backend, explained at `up` | no pod means no path guarantee and no collision safety; a degraded second dispatch path would be the weakest link everything else is judged by |
| Local-disk cache | rclone VFS on the **compute node**, write-back on close | a checkpoint over a network FS stalls the step loop, and a timer sweep is bloat. File close is the event. One mount then serves dataset caching, checkpoint write-back and re-reads |
| Dataset warming | lazy cache, `prefetch: true` opt-in | a run touching one percent of a tree should not copy all of it; a first epoch over a million small files is latency-bound and wants the copy |
| Transport | `via: auto` — direct, else relayed through this machine | node-to-node ssh is firewalled on many clusters and `AllowAgentForwarding no` is common, so direct cannot be assumed and a fallback must be *explained* rather than time out |
| Remote credentials | forwarded ssh-agent socket, never a key | lets gpu05 read gpu03 as you, with nothing stored there and authority that dies with the connection |
| Compute-node footprint | `vpnode` + `rclone` in `~/.vp/bin`, asked once per host | caching to a node's local disk needs a process on that node; the alternative is not a smaller footprint but no feature. Still no credentials, no agent, nothing system-wide |
| Names | `vibepod` for the TUI, `vp` for verbs | one is opened, the other is typed constantly; `vpctl` was a mouthful for the common case |
| Language | Go | os/exec, PTY, sockets, goroutine stream-plumbing are first-class; process startup is negligible against RTT |

## 12. Open questions

1. **Sandbox hardening parity** — bwrap has years of hardening (`/proc` masking, device
   allowlists, `--die-with-parent`) that our own root construction must re-derive.
2. **How much the narrowed log costs in practice** — with the exec gate gone, a bundled
   binary's direct `execve` is unrecorded, including a destructive one against a mount
   (§3, *What this costs*). Is the dispatching shell's coverage enough in real sessions, or
   does something cheaper than seccomp — read-only mounts by default, FUSE-level write
   logging — have to make up the difference?
3. **rclone as a dependency** — vendor the binary, require it, or reconsider a custom
   Go FUSE once access patterns are known?
4. **Unprivileged userns on compute nodes** — now load-bearing rather than academic: with
   the fallback deleted, a node without it cannot be a backend at all. How often is it
   disabled on the clusters this is for, and is "ask your admin for one sysctl" a reasonable
   thing to require?
5. **Cache eviction on a compute node** — `cache:` bounds the size, but a shared node's
   local disk is contended and a job that fills it hurts other people. Evict LRU, refuse to
   start when the bound cannot be met, or write to a node-specific scratch that is already
   quota'd?
6. **Backend switch mid-command** — a session's live remote shell may be busy when `vp use`
   or `b` arrives. Queue the switch, refuse it, or open a second shell and leave the first
   running?
7. **`host_access` defaults** — ship per-agent presets so the first run is not empty?
8. **Reverse-mount transport** — sshfs slave over `ssh -R`, rclone serving sftp back
   through the tunnel, or a push-copy? Caching runs the opposite direction here.
9. **Bind-shim accumulation** — one mount per distinct binary. Is there a ceiling worth
   caring about, and do shims need eviction? Measured in practice: a full Claude Code
   session shims a handful, so this is not urgent.
10. **Live-shell recovery** — a session-bound remote shell is state that a link drop
   destroys. Reopen it silently at the last known cwd, or surface the gap, given §7a's rule
   about never silently re-running a partially-applied command?
11. **Lease duration defaults** — long enough that a closed laptop does not kill a training
   run, short enough that an abandoned pod does not sit on a shared node for a week. Is one
   number right, or does this want `lease: 24h` per host with a shorter default on machines
   flagged as shared?
12. **Adoption across a config change** — the next `up` adopts a node pod under its lease,
   but `vibepod.yaml` may have been edited meanwhile. Reconcile the adopted pod to the new
   desired state, or refuse adoption and make the user choose?
13. **`vp save` and hand-edited YAML** — writing live state back over a file with comments
   and ordering the user cares about. Round-trip the comments, write a separate lockfile, or
   only ever append?
14. **Daemon upgrades** — the daemon outlives the binary that spawned it, so after an
   upgrade the running one is stale. `doctor` reports the mismatch; should the daemon
   instead hand over, or refuse a client whose build differs?

## 13. Milestones

**v1 = M0-M2, built. v2 = M3-M7, not started.** M6a is the first distributed piece and is
split out deliberately, so a single-node pod is trusted before a control plane sits on it.

- **M0 — the trick works. [done, then superseded]** Pod, seccomp exec gate, lazy bind-shim
  redirect, local binds only. It did work: Claude Code ran inside it with every exec
  intercepted and cwd tracking intact. §3 records why the mechanism was removed anyway —
  the assumption it validated was the wrong assumption.
- **M1 — remotes. [done]** Daemon, FUSE mount with execution-aware invalidation, cwd
  routing, warm ControlMaster. Plural structures throughout; one remote end to end.
- **M2 — lifecycle. [done — v1 shipped here]** `vpinit`, detach/attach, PTY buffer, the
  event stream, `log`, `tree`, the console, multiple sessions, `ps`/`down`, `doctor`.

**v2 is a turn, not a continuation.** M3-M5 below assume §3's backend model. They are
ordered so that each one is usable on its own.

- **M3 — backends.** Not started. Delete the exec gate, `vpsh`-as-shim, the bind-shim
  machinery and most of `gate.go`. Add: per-session backend state, one live shell per
  session per backend, the dispatching `$SHELL`, `/vp/bin` wrappers replacing `remote_tools`
  shims, `vp use` / `vp backend` / `vp @host`. Success is `cd` persisting in an attached
  gpu03 session, and the log still showing every shelled command.
  *Net negative lines, which is the point.*
- **M4 — live config.** Not started. `vp mount` / `vp unmount` into a running pod, a
  swappable route table, `vp hosts` listing unmounted machines, `vp save`, the mount
  allowlist. Mechanically cheap — §9 lists the four pieces that already exist — and it is
  what lets an agent add a machine without losing its own context.
- **M5 — the TUI.** Not started. The cockpit of §9: machines, sessions, activity, and
  handoff on `⏎`. `console.go` is the rough draft; the new work is the handoff and the
  keymap.
- **M6 — node pods.** Not started. `vpnode` pushed on consent; a pod on one backend, built
  as that node's system layer with the composed zone over it; `requires:` checked at `up`;
  rclone on the node with a local-disk cache and write-back on close; `prefetch:`; `via:
  auto` with its probe and explanation; the forwarded-agent credential proxy. Also `toolbin`
  pushes and port forwards, which share the consent path.
  Native binds for mounts the node owns, cached mounts for the rest.
  Success is a training run whose data lives on gpu03, whose GPUs are gpu05's, and whose
  every path is the same string on all three machines.
  **Deliberately excludes the control plane** — one node, mounts fixed at `up`. §6a's
  replication is M6a, so that the single-node case is working and trusted before anything
  distributed is built on it.
- **M6a — the control plane.** Not started, and the first genuinely distributed piece.
  Per-mount generations, reconcile-on-reconnect, per-mount staleness surfaced in `vp ps` and
  the TUI, dispatch refusal on a stale mount, append-only runtime ordering with the shadow
  guard, two-phase unmount with the dirty-flush refusal, the lease, and adoption on
  reconnect. Plus what replication does to the cache: per-file write tokens, invalidation
  fanned out per mount to every holder, the origin-overlap refusal, and the connection
  budget — §6a, *Many mounts, many caches*. This is what makes `vp mount` safe with more than one backend; until it lands,
  a second backend means `down` and `up`.
- **M7 — polish.** Not started. Reverse mounts (`expose_to:`) for "edit here, run there",
  `sync` mode, and the rest of the tree's views from §9: `-x` to expand a remote subtree, `--running`,
  `--failed --since`, one subtree by pid, `--mounts`/`--exec`.

### Gaps inside v1's own surface

Specified here, not built in v1. Two of the three are absorbed by the v2 work above; the
first is not, and matters more in v2 than it did in v1.

- **The generated `CLAUDE.md` fragment** (§9, and a locked decision in §11). Half of
  "knowing where things ran" — the half aimed at the agent — was never built. In v1 the
  agent inherited routing whether it knew or not, so this was a nicety. **In v2 it is the
  primary mechanism**: with no exec gate, an agent that has not been told about `vp` will
  simply run everything in the pod, over FUSE, on the wrong machine. Build it in M3, not
  after.
- **`VIBEPOD_TARGET`** (§9) — never exported, so a prompt cannot show the target without
  asking. Becomes `VIBEPOD_BACKEND` and falls out of M3.
- **The `@host cmd` per-command override** (§5) — `@` ended up naming machines for
  navigation instead, and the one-off override had no spelling. Specified in M3.

### Built after the plan

Not in any milestone above, because the work found them rather than the other
way round. Recorded so the document is not behind the code:

- **`remote_tools:`** — a shim needs a binary to shadow, so a tool that exists
  only on a remote had no way to be routed at all.
- **`exec.forward_env`** — routed commands were losing the caller's
  environment silently (§3).
- **Navigation by machine** — `vp hosts`, `vp where`, `vp cd @host`,
  because path identity makes paths too long to type.
- **Setup progress and explained ssh failures** (§9) — a silent ten-second
  wait on an unreachable host was indistinguishable from a hang.
- **A daemon build check** — the daemon outlives the binary that spawned it and
  spawns `vpinit` from its own image, so a rebuild changed nothing until the old
  one went away.
