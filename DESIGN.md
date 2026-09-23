# vibepod — design

> Status: **v1 built and verified** — M0-M2 (§13). See PLAN.md for what was
> measured before building, the six bugs verification found, and three
> deviations from this document that the implementation forced. Remaining
> unknowns in §12.

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
| **Composition** | unify local dirs + remote dirs into one filesystem view | pod mount namespace |
| **Identity** | agent config, skills, MCP, API keys | bound from host, never crosses the wire |
| **Execution** | decide which machine a command actually runs on | routing layer |

Planes 1 and 2 are plumbing. Plane 3 is the product.

## 3. The mechanism

Two parts: an **exec gate** that sees every program launch, and **lazy bind-shims** that
do the redirecting.

### Why not intercept the shell

The obvious design — bind `vpsh` over `/bin/sh` and forward the `-c` string verbatim —
does not survive contact with a real agent. Measured, in this repo:

```
$SHELL = /usr/bin/zsh                      # not /bin/sh, not /bin/bash

/usr/bin/zsh -c 'source ~/.claude/shell-snapshots/snapshot-zsh-*.sh 2>/dev/null || true
                 && setopt NO_EXTENDED_GLOB ... 2>/dev/null || true
                 && eval '"'"'<the actual command>'"'"'
                 && pwd -P >| /tmp/claude-XXXX-cwd'
```

Each call is a fresh `zsh -c`, not a persistent shell. Environment continuity comes from
replaying a snapshot file; cwd continuity comes from writing `pwd -P` to a **local** temp
file that the next call reads. Ship that string to a remote and:

- the snapshot path does not exist there, so the environment silently evaporates
- `setopt` is a zsh builtin, so the remote needs zsh
- `pwd -P >| /tmp/claude-XXXX-cwd` **writes on the wrong machine**, so `cd` stops
  persisting between calls — silently
- the whole string is zsh syntax handed to whatever shell the remote has

This is the default path for the primary agent, not a corner case. The shell must stay
local, where its wrapper, builtins, redirections, and cwd tracking all work untouched.
Interception belongs one level down, at the program.

### The exec gate

`vpinit` installs a seccomp filter with `SECCOMP_RET_USER_NOTIF` on `execve`/`execveat`,
passes the listener fd to `vibepod` over `SCM_RIGHTS`, then drops its capabilities. The
filter is inherited by every descendant, so the daemon observes **every exec in the pod** —
including statically-linked and agent-bundled binaries that no `$PATH` shim could catch.

This requires `CAP_SYS_ADMIN` in the pod's user namespace. `vpinit` receives it through
the **ambient** capability set when the daemon clones it, scoped to the pod's userns and
never the host. It keeps the capability — lazy bind-shims need it for the pod's whole life
— but clears the *ambient* set immediately, so every process it spawns, the agent
included, has an empty capability set and cannot mount, unmount, or unshim anything
(verified: `CapEff: 0`, child `mount()` → `EPERM`).

### Lazy bind-shims

seccomp-notify is a **gate, not a rewriter** — the supervisor may allow, deny, or
`CONTINUE`, but cannot alter `execve`'s arguments. The redirect uses the one thing notify
does provide: it freezes the syscall while the daemon decides.

```
agent execs /usr/bin/cargo
  → notify fires; the process is frozen mid-syscall
  → daemon: cargo, cwd /srv/api → prod. No shim at that path yet.
  → daemon asks vpinit to stash the original, then bind vpsh over /usr/bin/cargo
  → reply CONTINUE
  → the kernel resolves the path now, and finds the shim
```

Path resolution happens after the syscall resumes, so the bind lands in time. First exec
of a binary pays one mount (~1ms); every exec after is free. No `$PATH` enumeration, no
shim-set staleness, and no ptrace — which matters, because ptrace is exclusive and would
break `strace` and `gdb` *inside* the pod.

Where seccomp-notify is unavailable (older kernels, nested containers), this degrades to a
statically generated shim directory built from the remote's own `$PATH`. Same daemon-side
policy, weaker coverage.

### What crosses with a routed command

Interception keeps the shell local, but a command still has an environment, and
the question of which parts of it follow the command to another machine is a
separate one with a different answer.

**Blanket forwarding is refused**, for two reasons that are both disqualifying
on their own:

- A pod's environment holds `ANTHROPIC_API_KEY`, `HF_TOKEN` and their
  relatives. §7 promises those never leave this machine. Sending the
  environment sends them.
- `PATH`, `HOME`, `LD_LIBRARY_PATH`, `PYTHONHOME`, `SSH_AUTH_SOCK` and the
  `XDG_*` family describe *this* machine. Imposing them on a remote breaks its
  toolchain in ways that are harder to diagnose than the thing they were meant
  to fix — a wrong `PATH` is worse than a missing variable.

**Sending nothing is also wrong**, and fails silently, which is worse. A
dropped `PYTHONPATH` surfaces as `ModuleNotFoundError`, which reads as a broken
install rather than a discarded environment; the user is sent to debug the
wrong machine.

So what crosses is the **delta**: the difference between the command's
environment and the environment its session started with. That is exactly
`VAR=value cmd`, and `export VAR=...` earlier in the same shell, and nothing
else — because anything a user did not touch is, by construction, identical to
the baseline. Identity variables are excluded from the delta even when set
deliberately, and **a refusal is reported rather than swallowed**: it goes to
the event stream as a notice, where `vpctl log` shows it. Silence is how the
original bug survived.

Assignments are emitted before `exec` in the remote script, not after — `exec
VAR=v cmd` would have the shell look for a program named `VAR=v` — which also
leaves the pid the shell's own, so the pid file and the signal path are
untouched.

`exec.forward_env` can set this to `none`, or to an explicit list of names for
anyone who would rather say precisely what travels.

### What this buys

- **Agent wrappers work untouched.** The wrapper, `source`, `setopt`, and the `pwd -P`
  capture all run locally, so cwd tracking keeps working.
- **`cd /srv/api` is a local operation** against the mount, and needs no special handling.
- **Pipelines split naturally.** `rg foo | head -20` runs `rg` on the remote and `head`
  in the pod, streaming between them — better than routing the whole pipeline one way.
- **Direct execs are caught.** The agent's built-in Grep spawns ripgrep without a shell;
  under shell-level interception that read would have gone over FUSE.

Known cost: a redirection like `cmd > out.txt`, where `out.txt` sits in a FUSE mount, has
the remote program's stdout streamed back and written locally over FUSE. Correct, slower
than native, optimisable later.

### Terminal and signals

SSH does not forward `SIGINT` without a PTY, so Ctrl-C on a routed `make` would otherwise
leave an orphan on the remote. But `-tt` merges stderr into stdout and mangles binary
output, and agents parse those streams separately.

So: **piped by default**, with signals forwarded by killing the remote *process group* over
a second multiplexed channel (~5ms, pure POSIX, nothing installed remotely). **PTY only
when vpsh's own stdio is a tty** — which is exactly `vpctl shell` and interactive programs.

## 4. Architecture

```
┌─ your machine ─────────────────────────────────────────────────┐
│                                                                │
│  vpctl ──unix socket──► vibepod  (rootless user daemon)        │
│                            ├─ pod registry & lifecycle         │
│                            ├─ route table (path → host:path)   │
│                            ├─ ssh ControlMaster pool           │
│                            ├─ PTY buffers (dtach-style)        │
│                            ├─ credential proxy                 │
│                            └─ audit log                        │
│                            ▲                                   │
│  ┌─ pod "work" (bwrap namespace) │                             │
│  │   vpinit (PID 1) ─────────────┤  holds ns; owns exec gate   │
│  │     └ seccomp notify fd ──────┤  every execve, to the daemon│
│  │   vpsh (bind-mounted lazily) ─┘  over intercepted binaries  │
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

### Components (one Go binary, four names via argv[0])

| Name | Role |
|---|---|
| `vpctl` | thin client. `up`, `ps`, `shell`, `run`, `attach`, `exec`, `down` |
| `vibepod` | rootless user daemon, auto-spawned on first use. Owns everything long-lived |
| `vpinit` | PID 1 inside each pod. Holds the mount namespace open, installs the seccomp exec gate then drops caps, reaps zombies, forwards signals |
| `vpsh` | the shim, bind-mounted over intercepted binaries on demand. Forwards `(cwd, argv, env, fds, tty)` to the daemon, proxies exit code |

`vpinit` exists because **a mount namespace only survives while a process is inside it.**
Without it, detaching would destroy the pod.

## 5. Routing

**cwd decides the machine.** Resolved in precedence order:

| # | rule | source |
|---|---|---|
| 1 | session pin | `vpctl use <host>`, inherited via `VIBEPOD_EXEC` |
| 2 | mount's `exec_on:` | config — the durable "this directory runs there" |
| 3 | mount owner | remote mount → its host · local mount → the pod |
| 4 | `exec.default` | config, for cwd matching no mount |

Per-command override: `@prod cmd`, `@local cmd`, `@pod cmd`.

### Multiple hosts

A pod mounts from and executes on **any number of hosts at once**. Nothing in the model is
singular: the route table is a `path → (host, path)` map, the SSH mux pool is keyed by
host, `toolbin` and `forward_credentials` are already per-host, and `vpctl tree` carries a
host column. A single-host special case would have to be deliberately added, and then
removed again.

What is genuinely extra for several hosts is narrow: `expose_to: [a, b]` means one reverse
transport per target, and a command spanning two hosts picks one side (§3). M1 exercises a
single remote to keep the first integration small — a scoping choice, not a limit.

Rules 2 and 3 are both properties of the *directory*, which is what lets agents inherit
routing for free — they obey the same cwd rule everything else does, with nothing to
learn and no session state to track. `exec_on:` covers "my code is local, the machine
that should run it is not":

```yaml
  - local: ~/Git/proj
    expose_to: [gpu-box]     # gpu-box can see this directory
    exec_on: gpu-box         # commands whose cwd is here run on gpu-box
```

Rule 1 is the ad-hoc escape hatch for interactive work. It is inherited by child
processes, so `vpctl use gpu-box` followed by `claude` sends every command that agent
runs to gpu-box regardless of cwd. That is deliberate — it is the original `set_remote`
workflow — but it is also the one route the console's status bar cannot really protect
you from, since agent output scrolls faster than it can be read. Prefer `exec_on:` for
anything durable.

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
process reading `/usr/lib/...` silently gets remote files over FUSE. `vpctl up` prevents
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

Less than it first appears. Under routing, `rg`, `make`, and `cargo` execute **on the
remote against its local disk** and never touch FUSE. The FUSE load is only:

- the agent's built-in file tools (Read / Glob / Grep)
- MCP servers
- pod-local commands

The worst of these is the agent's own Grep running ripgrep over the mount — which is
precisely the direct exec the exec gate catches. **The FUSE performance risk is mostly the
interception-gap problem wearing a different hat**; closing one closes the other.

### Backend: rclone sftp with a VFS cache

| | sshfs 3.7.6 | **rclone sftp + `--vfs-cache-mode full`** | custom Go FUSE |
|---|---|---|---|
| status | mature, maintenance-mode | actively developed | ours |
| second read | round trip | local disk | whatever we build |
| invalidation hook | none clean | `vfs/forget` via rc API | exact |
| effort | none | small | large |

The deciding factor is the invalidation hook — see below. `rclone` is **not** installed on
this machine, so it is a real dependency to vendor or require.

### Execution-aware cache invalidation

A general-purpose network filesystem must guess: it caches for a second because it has no
idea what is happening on the other end.

**We are not guessing.** vibepod mediates every command, so it knows exactly when the
remote tree could have changed — nothing else touches it. That permits effectively
infinite attribute and entry timeouts, with invalidation driven by **command completion**
rather than a timer. It is a correctness-preserving cache far more aggressive than any
network FS can justify, and it exists only because of the routing layer.

This is why the backend needs a `vfs/forget`-style hook. sshfs has none.

### Reverse mounts (`expose_to:`)

Mounts flow both ways. `expose_to: [host]` on a local directory makes it visible **on**
that host, so a command routed there can see local code. Without it, "edit locally, run
on the big machine" is impossible — the target has no such path.

```yaml
  - local: ~/Git/proj
    expose_to: [gpu-box]
```

`exec_on:` without a matching `expose_to:` is a misconfiguration and is rejected at `up`.

Transport is undecided (§12): sshfs slave mode over `ssh -R`, rclone serving sftp back
through the tunnel, or a push-copy. Note the asymmetry — for a reverse mount, the
*remote's* reads become network reads, so the caching story runs the opposite direction
from a normal mount.

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

**Remote footprint.** Nothing is installed speculatively. When a routed command needs a
missing tool (`rg`, `fd`, `jq`), vibepod prompts before pushing a static binary to
`~/.vibepod/bin`. `toolbin: true` pre-authorizes a host so agent runs aren't interrupted.
Removable with one `rm -rf`.

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

It does not silently retry. A routed `make deploy` or migration that already partially
executed must not be re-run behind the agent's back — a visible failure the agent can
reason about is strictly better than a half-applied change it never learns about.

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
  - remote: prod:/srv/api          # → /srv/api in pod, commands here → prod
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

remote_tools:                      # exist only on a remote; shimmed in /vp/bin
  - rocm-smi                       # ahead of PATH, since nothing here to shadow

host_access:                       # bound from host into pod
  - ~/.claude
  - ~/.config/opencode

ports:
  - prod:3000                      # auto -L forward

exec:
  default: pod                     # when cwd matches no mount
  forward_env: delta               # what the caller set, only (§3); or none,
                                   # or an explicit list of names
```

Split `vibepod.yaml` (committed, shareable) from `vibepod.local.yaml` (your paths,
gitignored).

## 9. Interface

### Command surface

```
vpctl new [name]            create pod + open the console
vpctl up [name]             create pod, detached, no console     (scripts, CI)
vpctl run claude [target]   create/attach and launch an agent
vpctl shell [pod]           another independent terminal into a running pod
vpctl attach [pod]          reattach to a detached console or agent session
vpctl hosts [pod]           the machines this pod runs on, and what they own
vpctl where [@host]         which machine runs this directory, or vice versa
vpctl cd @host              switch to a machine's directory (console only)
vpctl use <host|auto>       set this session's executor
vpctl exec @prod -- cmd     one-off, explicit target
vpctl ps                    list pods, routes, health
vpctl tree [pod]            pod structure: mounts and live exec tree
vpctl log [-f] [pod]        every exec and where it ran
vpctl down [pod]            stop, unmount, disconnect
vpctl doctor                check bwrap, seccomp, mounts, ssh reachability, toolchains
```

Pods are **named** and globally listable, but a bare `vpctl new`/`up` in a project
directory takes its name from `./vibepod.yaml`.

### The console

`vpctl new` opens a cockpit — not a shell replacement. Competing with zsh and tmux means
rebuilding completion, history, job control and a terminal emulator, and losing anyway.

```
┌─ vibepod · work ───────────────────────────────────────────────┐
│ exec: auto            /srv/api → prod       8ms    2 mounts ✓  │
├────────────────────────────────────────────────────────────────┤
│ 14:22:31  prod    cargo test                          ✓ 12.4s  │
│ 14:22:48  local   git commit -m "fix parser"          ✓  0.1s  │
│ 14:23:02  prod    rm -rf target                       ✓  0.3s  │
│                                                                │
│ /srv/api ❯ _                                                   │
└────────────────────────────────────────────────────────────────┘
```

Header: pod, executor mode, routes, health. Body: the live exec log. Bottom: a command
input. Anything needing a real TTY (`vim`, `htop`, `claude` itself) takes over the full
screen and hands it back on exit.

A pod supports **many concurrent sessions** — `vpctl shell` attaches another independent
terminal to a running pod, `docker exec -it` style. Each session has its own cwd and its
own executor pin.

### The tree

`vpctl tree` is the one view nothing else can produce. `pstree` stops at the machine
boundary; the exec gate sees every `execve` with its routing decision, so here **the
machine is a column**. It renders the pod's whole structure — what is mounted from where,
and what is executing where — and replaces a separate `vpctl mounts`.

```
$ vpctl tree work
work · running 3m12s · exec: auto

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
└─ session 2  (vpctl shell)
   └─ zsh                                 pod      8m40s
      └─ python train.py                  gpu-box  1m04s  ●
```

The two halves are deliberately in one view: **the mount tree explains the exec tree.**
"Why did that run on gpu-box" is answered four lines up, by the `exec_on` under
`~/Git/proj`.

Sessions are the roots, since a pod has several. Completed subtrees collapse to a count —
an hour of agent work is hundreds of execs, and an uncollapsed tree is unreadable.

**Remote depth.** We gate execs in the pod, not on prod, so a routed command is a **leaf**:
the `rustc` and `ld` that `cargo` spawns on prod are invisible. `vpctl tree -x` polls
`ps --ppid` over the warm mux to expand a remote subtree on demand, marked as polled and
approximate. Honest by default, deep when asked.

**One data source, three renderings.** The daemon emits a single event stream (exec start,
exec exit); the console pane, `vpctl tree -f`, and `vpctl log -f` are all subscribers.
Collection is solved by the exec gate — this is only a rendering problem.

```
vpctl tree --json                 # frugal: live in full, completed as counts
vpctl tree --json --all           # everything the daemon knows
vpctl tree --json -f              # NDJSON, one event per line
vpctl tree --running              # what is still alive
vpctl tree --failed --since 10m   # what broke recently
vpctl tree 412                    # one subtree
vpctl tree --mounts | --exec      # one half
```

**Agent-facing output is frugal by default.** The human view collapses completed subtrees
for readability; `--json` collapses them for context budget — an hour of agent work is
hundreds of execs, and a complete tree is tens of kilobytes spent on `rustc` invocations
nobody asked about. `--all` is there when something is genuinely parsing it.

Note that plain `vpctl tree` is already a fine agent interface, and a cheaper one — JSON
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

`vpctl log` and `vpctl tree` pair rather than overlap: **log is flat, chronological,
finished; tree is hierarchical, live, running.**

### The in-pod control plane

The agent runs *inside* the pod, where `vpctl` reaches the daemon through a socket bound
into the namespace. That socket is a control plane, and reading is not the same as writing.

| | pod socket | host socket |
|---|---|---|
| `tree`, `log`, `ps`, `where` | ✓ | ✓ |
| `use` (own session only) | ✓ *if the session has a tty* | ✓ |
| `down`, `allow`, `up` | ✗ | ✓ |

The tty gate on `use` is the important one. Without it **the agent can re-route itself** —
pin its own execution to a machine cwd would never have chosen, granting itself a
capability you did not give it. With it, you can still pin from `vpctl shell`, because a
human at a terminal has one and an agent's subprocess does not. A config flag opens it
deliberately for the cases that want it.

The tty check is a proxy for intent, not proof of it. It is cheap, it fails closed, and
the alternative — an explicit grant step — costs a round trip at exactly the moment you
are trying to work.

### Knowing where things ran

Two audiences, two mechanisms.

**You:** `vpctl log -f` is the trust surface. Since the daemon mediates every exec, it can
show the one thing nothing else can — what the agent is doing *and where*. It is a primary
surface, not a debugging afterthought.

**The agent:** vibepod writes a `CLAUDE.md` fragment into the pod, in the agent's own
language. No MCP server, no new tool to learn.

```
You are in a vibepod. Commands run on the machine that owns their directory:
  /srv/api      → prod     (remote)
  ~/Git/proj    → gpu-box  (local files, remote execution)
  ~/Git/notes   → local

Run `vpctl tree --json` to see what is executing and where.
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
         run `vpctl allow toolbin prod` to push it
```

### Guardrails

There are none, by choice. vibepod routes and records; it does not judge. The agent's own
permission system already gates commands, and pattern-matching shell strings for `rm -rf`
is leaky in both directions — false positives block real work, and evasion is trivial.
The log is the answer.

Non-invasive by default: `VIBEPOD_POD` and `VIBEPOD_TARGET` are exported and a prompt
snippet is opt-in, rather than rewriting anyone's `PS1`.

## 10. Detach & reattach

`vibepod` owns the PTY and keeps a scrollback ring buffer (dtach/abduco-style, built in —
no tmux dependency). Detaching leaves the agent running; `vpctl attach` reconnects and
replays the buffer. Killing the pod kills everything inside it.

## 11. Decisions locked

| Decision | Choice | Why |
|---|---|---|
| Pod backend | own user+mount+pid namespace | ~10ms start, no image, reuses host binaries. bubblewrap cannot host this design — see below |
| Topology | central `vibepod` daemon + per-pod `vpinit` | one socket, one audit log, shared ssh muxes; `vpinit` covers the namespace-lifetime requirement |
| Privilege | rootless, auto-spawned | credentials are user-owned; root buys nothing and costs the security story |
| Exec routing | cwd-inferred + `@host` override | no invisible mode state; agents get it right with zero prompting |
| Interception | seccomp exec gate + lazy bind-shims | agents wrap commands in generated shell scripts; the shell must stay local. Catches bundled and static binaries that `$PATH` shims cannot |
| Redirect | bind-mount `vpsh` while notify holds the syscall | notify is a gate, not a rewriter; ptrace would break `strace`/`gdb` inside the pod |
| Sandbox | native `clone` + `pivot_root`, behind an interface | bwrap nests a *second* userns after building the root, so nothing inside can mount — fatal to lazy bind-shims. Measured, not assumed |
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
| Remote-only tools | `remote_tools:`, shimmed into `/vp/bin` ahead of `PATH` | a shim needs a binary to shadow, and the GPU box's own tools are not on your laptop |
| Host access | explicit allowlist | nothing granted implicitly; the agent cannot read unrelated projects or credentials |
| Link drops | fail loudly, exit `75` | never silently re-run a partially-applied non-idempotent command |
| Console | cockpit: status + log + input | a command surface that shows routing, without rebuilding a shell |
| Navigation | by machine (`vpctl cd @host`), never plain `cd` | path identity makes paths long; a directory can be named `@host`, and a `cd` that guessed would silently change machines |
| Prompt | full path plus the machine it runs on | the path is the honest cost of path identity; the machine is what you need before pressing return |
| Sessions | many per pod | `vpctl shell` attaches independent terminals, `docker exec -it` style |
| Exec target | `exec_on:` on a mount, session pin overrides | keeps "cwd decides" as the one rule; agents inherit routing with nothing to learn |
| Reverse mounts | `expose_to:` on local mounts | "edit locally, run on the big machine" is impossible without them |
| Visibility | live exec log + generated `CLAUDE.md` | one mechanism per audience; no output annotation to corrupt parsed streams |
| `vpctl tree` | mounts and exec structure in one view | the mount half explains the exec half; replaces a separate `mounts` command |
| Remote depth | leaf by default, `-x` polls `ps` | we gate execs in the pod, not on the remote; do not fake fidelity we lack |
| In-pod scope | read-only, plus own-session `use` behind a tty check | stops the agent re-routing itself while keeping `vpctl shell` usable |
| Routed env | forward the caller's delta; never identity, never credentials | blanket forwarding breaks §7's promise and the remote's toolchain; sending nothing fails silently as a broken install |
| Guardrails | none — the log is the answer | pattern-matching shell strings is leaky both ways; the agent already gates commands |
| Hosts | ssh_config aliases | inherits ProxyJump/keys/ports for free |
| Language | Go | os/exec, PTY, sockets, goroutine stream-plumbing are first-class; ~3ms vpsh startup is negligible against RTT |

## 12. Open questions

1. **Sandbox hardening parity** — bwrap has years of hardening (`/proc` masking, device
   allowlists, `--die-with-parent`) that our own root construction must re-derive.
2. **Seccomp availability** — `TSYNC|TSYNC_ESRCH|NEW_LISTENER` needs Linux 5.7+. What is
   the floor we support, and does the static-shim fallback carry its weight?
3. **rclone as a dependency** — vendor the binary, require it, or reconsider a custom
   Go FUSE once access patterns are known?
4. **Reverse mounts to several targets** — `expose_to: [a, b]` needs one transport per
   target. Worth supporting, or is one target per local mount enough?
5. **`sync` mode implementation** — rsync loop, or a mutagen-style watcher?
6. **`@host` prefix parsing** — with the shell now local, where does the override live?
7. **`host_access` defaults** — ship per-agent presets so the first run is not empty?
8. **Reverse-mount transport** — sshfs slave over `ssh -R`, rclone serving sftp back
   through the tunnel, or a push-copy? Caching runs the opposite direction here.
9. **Bind-shim accumulation** — one mount per distinct binary. Is there a ceiling worth
   caring about, and do shims need eviction? Measured in practice: a full Claude Code
   session shims a handful, so this is not urgent.
10. **Exit status for pod-local commands** — vibepod observes their execs but does not
   own them, so it never learns what they returned. Worth a `PTRACE_O_TRACEEXIT`-style
   mechanism, or is "we only report what we know" the right answer?
11. **Daemon upgrades** — the daemon outlives the binary that spawned it, so after an
   upgrade the running one is stale. `doctor` reports the mismatch; should the daemon
   instead hand over, or refuse a client whose build differs?

## 13. Milestones

**v1 = M0-M2.**

- **M0 — the trick works. [done]** bwrap pod, seccomp exec gate, lazy bind-shim redirect, local
  binds only. Success is running Claude Code inside it and seeing every exec intercepted
  with cwd tracking intact. This is the riskiest assumption in the design.
- **M1 — remotes. [done]** Daemon, rclone mount with execution-aware invalidation, cwd routing,
  warm ControlMaster. Plural structures throughout; one remote exercised end to end.
  First genuinely useful version.
- **M2 — lifecycle. [v1 ships here — done]** `vpinit`, detach/attach, PTY buffer, the
  event stream, `log`, `tree`, the console, multiple sessions, `ps`/`down`.
- **M3 — real work.** Not started. Credential proxy for a routed `git push`,
  `toolbin` pushes, port forwards, reverse mounts (`expose_to:`).
  **`exec_on:` depends on M3 and is a trap until then**: the config accepts it,
  but without the reverse mount the target cannot see the directory, so it fails
  at run time rather than at `up`. Either implement `expose_to:` or refuse
  `exec_on:` at `up` — the current middle is the one thing §9 says not to do.
- **M4 — polish.** Not started. `expose_to` to several targets, `sync` mode,
  and the rest of the tree's views from §9: `-x` to expand a remote subtree,
  `--running`, `--failed --since`, one subtree by pid, `--mounts`/`--exec`.
  (`doctor` shipped in v1 and has moved out of here.)

### Gaps inside v1's own surface

Things this document specifies and v1 does not do. Each is small; listing them
is cheaper than rediscovering them.

- **The generated `CLAUDE.md` fragment** (§9, and a locked decision in §11).
  Half of "knowing where things ran" — the half aimed at the agent — was never
  built. The log serves a human; nothing currently tells the agent, in its own
  language, that its directories map to machines.
- **`VIBEPOD_TARGET`** (§9). `VIBEPOD_POD` and `VIBEPOD_SESSION` are exported;
  the target never was, so a prompt snippet cannot show it without asking.
- **The `@host cmd` per-command override** (§5, open question 6). `@` ended up
  naming machines for navigation instead. A one-off override still has no
  spelling, and `vpctl exec @prod -- cmd` from the §9 command surface does not
  exist.

### Built after the plan

Not in any milestone above, because the work found them rather than the other
way round. Recorded so the document is not behind the code:

- **`remote_tools:`** — a shim needs a binary to shadow, so a tool that exists
  only on a remote had no way to be routed at all.
- **`exec.forward_env`** — routed commands were losing the caller's
  environment silently (§3).
- **Navigation by machine** — `vpctl hosts`, `vpctl where`, `vpctl cd @host`,
  because path identity makes paths too long to type.
- **Setup progress and explained ssh failures** (§9) — a silent ten-second
  wait on an unreachable host was indistinguishable from a hang.
- **A daemon build check** — the daemon outlives the binary that spawned it and
  spawns `vpinit` from its own image, so a rebuild changed nothing until the old
  one went away.
