# vibepod

Run your code agent on your own machine, against code that lives on other
machines, and choose where each command runs.

Working on a remote server over SSH usually means one of two bad trades: install
Claude Code, Codex or OpenCode on every remote and copy your API keys, MCP config
and skills to each — or work locally and lose the remote's toolchain, data and
services.

vibepod inverts it. A *pod* is a namespace that composes three things:

| | |
|---|---|
| **Composition** | local and remote directories in one filesystem view — **the same view on every machine** |
| **Identity** | your agent config, skills and credentials, bound from this host — **never transmitted anywhere** |
| **Execution** | each session runs on a machine you choose, and can move between them |

The first two are plumbing. The third is the point.

```console
$ vibepod                       # the cockpit for this project's pod
$ vp @gpu03 rocm-smi            # one command over there
$ vp use gpu03                  # move this session; later commands go there
$ vp log -f                      # every command, and where it ran
```

## The machine is chosen, not guessed

v1 inferred the machine from the working directory, through a seccomp filter on
`execve` that redirected commands with bind-mounted shims. It worked, it was
built, and it was removed. `DESIGN.md` §3 is the argument; the short version is
that a redirected `execve` is not a process, it is an `ssh`, and the list of
things it silently loses — `/tmp` identity, descriptors past the first three, the
process group, job control, rlimits, cgroups, signal delivery — does not
terminate. You can close any one gap and still never say "this is correct now",
so every command gets checked by hand, so it bought nothing.

What replaced it is smaller and says what it does:

- Every session has a **backend**: `pod`, or a machine. `vp backend` answers in a
  word, `vp use` moves it, `@machine` sends one command elsewhere.
- Attach to a session whose backend is `gpu03` and you are **in a real shell on
  gpu03** — not a proxy, not one ssh per command. `cd` sticks because it is `cd`.
  `export`, `jobs`, `fg` and history all behave, because none of it is being
  reproduced.
- The pod's `$SHELL` records every command line before running it, so `vp log`
  still shows what happened and where. It is bypassable by a direct `execve`, and
  that is the trade: it catches what agents and humans actually do at a fraction
  of a kernel gate's cost.

## Every backend runs a pod

gpu03 has the data, gpu05 has the GPUs, and gpu05 has never heard of gpu03. Send
a command to gpu05 with a working directory of `/remote/vast0/…/proj` and either
the path is missing — fine, it fails — or **it exists and holds different bytes**,
which succeeds, writes somewhere real, and is discovered days later.

So vibepod does not translate paths. It reproduces them: the first command sent
to gpu05 builds a pod *on gpu05* holding the same composed tree at the same
absolute paths (`vp node add gpu05` does it ahead of time). What crosses is one binary in `~/.vp/bin` and a description of the mounts.
No credentials, no agent, nothing installed outside that directory, and `vp node
drop` removes what it made.

Everything else in a node pod is the node's **own**: its toolchain, devices, home,
conda and module trees, because that difference is the reason to dispatch there
at all — a minimal root on a GPU box hides the GPUs and reads as "ROCm is
broken". The composed tree is placed over it without writing to the machine. A
mount can say what it needs of whoever runs it:

```yaml
  - remote: gpu03:/remote/vast0/duongnguyen/imagenet
    requires: [/opt/rocm, /dev/kfd]   # a machine lacking these is refused
    cache: 200G                        # on that machine's own disk
    prefetch: true                     # many small files: fill it up front
```

A mount the node owns is a native bind — no FUSE, no cache, no round trip — so
running work where the data lives is full speed with nothing to configure.

## The config is a live object

Adding a machine used to mean `down`, edit the file, `up`, and losing every
session and the agent's context with them. Now:

```console
$ vp mount gpu05:/remote/vast0/shared        # into a running pod
$ vp unmount @gpu05
$ vp save                                    # write the live state back
```

Anything in `vibepod.yaml` is changeable while the pod runs, and the file becomes
a snapshot you take rather than something you restart for.

Mounting is the one thing the in-pod control plane does not leave unrestricted:
it opens a network path out of a sandbox built for containment, so an agent may
mount hosts in your ssh config (or your `can_mount:` list) and nothing else.

## What never leaves your machine

- **API keys and agent credentials.** Bound into the pod; never transmitted. A
  node pod's spec has no field for them.
- **SSH private keys.** The pod cannot see them at all — the daemon runs outside
  every namespace and owns every connection. A node uses its own ssh config.
- **Anything you did not name.** Only paths in `host_access:` are bound in, so an
  agent working on a client's code cannot read `~/.aws` or a sibling project.

A dispatched command carries what you set for it — `VAR=v cmd`, or an `export`
earlier in the same shell — and nothing else. Not your credentials, and not your
`PATH` or `HOME`, which describe this machine and would break the remote's
toolchain. A variable declined for that reason is reported in `vp log` rather
than dropped silently.

Processes in a pod hold **no capabilities at all** — they cannot mount, unmount or
alter a mount. That said: a namespace contains accidents, not adversaries.

## Install

Needs Linux with unprivileged user namespaces, and `ssh`. For remote mounts,
`rclone` (preferred: it caches on local disk and can be told when to forget) or
`sshfs`. No root, ever.

```console
$ make install          # ~/.local/bin/{vibepod,vp,vpsh}
$ vibepod doctor
```

## Configure

`vibepod.yaml`, next to your project. Hosts are **ssh_config aliases**, so
ProxyJump, keys and ports are inherited rather than reimplemented.

```yaml
pod: work

hosts:
  gpu05: {}                    # a machine with no data of its own: pure compute

mounts:
  - remote: gpu03:/remote/vast0/duongnguyen/proj
  - local: ~/Git/notes
  - remote: gpu03:/var/log/api
    readonly: true

remote_tools:                  # commands that belong on another machine
  gpu05: [rocm-smi, hipcc]     # a wrapper in /vp/bin dispatches them by name

host_access:                   # bound from this host, never transmitted
  - ~/.claude
  - ~/.claude.json

can_mount: ["gpu*"]            # what `vp mount` may reach from inside the pod

exec:
  default: pod                 # the machine a new session opens on
```

A remote directory mounts at **its own absolute path**. That is not cosmetic:
arguments forward verbatim, a compiler error naming `/remote/vast0/…/db.rs:14` is
a path anything can open, and moving a session between machines does not move the
ground under your prompt. `up` refuses a mount that would shadow a system path.

Two ready-to-run configs are in `examples/`: `local/` needs no network, `gpu/`
mounts a directory from a real remote.

## Use

```
vibepod [pod]                 open the cockpit
vibepod up [name] [--push]    create the pod, detached
vibepod down [pod]            stop it, unmount, disconnect
vibepod doctor                check this machine can host a pod

vp backend                    which machine is this session on
vp use <machine|pod>          move it
vp @<machine> cmd ...         run one command there
vp hosts                      the machines this pod knows, mounted or not
vp node [add|drop <machine>]  the machines running a pod of their own
vp mount <host>:<path> [at]   connect and mount into a running pod
vp unmount <path|@machine>    the reverse
vp save                       write the live state back to vibepod.yaml
vp where [@machine]           what this directory is called there, or the reverse
vp tree                       the mounts, and what is running on which machine
vp log [-f]                   every command, and where it ran
vp ps                         pods, sessions, backends
vp shell [pod] [-on machine]  another terminal on a running pod (--raw: no blocks)
vp attach [pod] [session]     return to a session you detached from
vp run [pod] -- cmd ...       one command in a pod, creating it if needed
vp brief                      the instructions the agent in this pod was given
```

`Ctrl-\` detaches and leaves the session running. Pods are named and outlive the
terminal that made them, like containers. A terminal that disappears without
detaching is also a detach: an agent halfway through something is not killed by
the disappearance of the thing that was watching it.

## vp shell

`vp shell` is a terminal in the style of Claude Code and Codex: a line to type
at the bottom, and each command's output above it as a block that says which
machine it ran on, in which directory, and how it ended.

```
▌ $ source .venv/bin/activate                         ~/gpu-intern-26 · gpu03
▌ $ python train.py --epochs 3                        ~/gpu-intern-26 · gpu03
▌ epoch 1  loss 0.93
▌ epoch 2  loss 0.71
▌ ✓ 41.2s

╭──────────────────────────────────────────────────────────────────────────────╮
│ ❯ run on gpu03                                                               │
╰──────────────────────────────────────────────────────────────────────────────╯
 gpu03  ~/gpu-intern-26           / commands · tab complete · ctrl+\ detach
```

Underneath it is the same login shell as before, in the pod or over ssh, so
`cd`, `export`, `source` and your aliases carry from one command to the next.
When it first sees a shell (bash or zsh), vibepod types a few hooks into it
that mark where each command's output begins and ends. Nothing is installed,
and your prompt, rc files and history stay yours. A block's bar is coloured by
machine. A finished block goes into the terminal's own scrollback, so scrolling,
searching and copying work, and it is still there after you quit. The input box
stays on the bottom rows, and output fills the screen from just above it, the
way a plain terminal scrolls. It starts on a clean screen, with what was there
scrolled up rather than erased, and `clear` clears the terminal rather than just
its own block.

**Tab** completes the way the shell on that machine would: zsh's completion
system or bash-completion, run where the session is and in its directory, so on
gpu03 it offers gpu03's files and commands. **PgUp** and the mouse wheel scroll
back; inside tmux they go straight into copy mode, with no prefix key, and
scrolling back to the bottom leaves it.

A line that starts with `/` and a name below is vp shell's own command; any other
line goes to the shell, so `/usr/bin/ls` still runs. A leading space sends even
these to the shell. Typing `/` shows them.

| | |
|---|---|
| `/use <machine>` | move this session — the way back from a machine with no `vp` on it |
| `/hosts` | the machines this pod knows |
| `/clear` | clear the screen and the scrollback |
| `/detach`, `/exit` | as ctrl+\ and ctrl+d |
| `/help` | commands and keys |

While a command runs, every key goes to it, and its output is drawn by a
terminal emulator. That means progress bars, a Python REPL, a password prompt
and inline interfaces like Claude Code's all work. A program that takes the
whole screen (vim, htop, less) gets the real terminal until it leaves. `/use
gpu03` moves to that machine's shell, and `/use pod` comes back.
`ctrl+\` detaches, and `vp attach` brings the blocks back, including a command
that was still running. `vp shell --raw` is the shell's own pty, without any of
this. So is a shell whose hooks do not take (fish, for now), and it says so.

## The cockpit

`vibepod` with no arguments:

```
╭─ vibepod · work ───────────────────────────────────────────────────────────╮
│ MACHINES                        │ ACTIVITY                                 │
│ ❯ ● pod*     ~/Git/notes        │ ▌ 14:02 gpu03  python train.py         ⠋ │
│   ● gpu03    /remote/vast0/…    │ ▌ 14:01 pod    rg TODO           ✓ 0.3s  │
│   ○ gpu05    vp mount gpu05:/…  │ ▌ 13:58 gpu03  rocm-smi          ✓ 1.2s  │
│                                 │                                          │
│ SESSIONS                        │                                          │
│ › 1  claude  ▸ pod              │                                          │
│   2  shell   ▸ gpu03            │                                          │
│                                 │                                          │
│ MOUNTS                          │                                          │
│   /remote/vast0/…/proj    gpu03 │                                          │
╰─ mount gpu05:/data — done ─────────────────────────────────────────────────╯
 pod  session 1       m mount · u unmount · b backend · ⏎ attach · q quit
```

It is drawn in the same language as `vp shell`: a machine has one colour in
both (the dot, its activity bar, its blocks), `❯` marks where the keys act,
and the last thing the cockpit had to say sits in the bottom edge.

`⏎` does not draw a terminal inside a pane. It **leaves the alt-screen, runs
`vp attach` on the real terminal, and redraws when you come back** — what
lazygit does with `$EDITOR`. So a session `vp shell` started comes back as
blocks, and any other session comes back raw. Running an agent inside a homemade multiplexer means nested
alt-screens, mouse reporting fighting mouse reporting, mangled bracketed paste
and resize storms; handoff costs a tenth of the work and has none of those
failure modes. The detach key therefore belongs to the session, not to the
cockpit: swallowing keystrokes an agent wants is exactly the failure being
avoided.

## Telling the agent

There is no exec gate any more, so an agent that has not been told about `vp`
will run everything in the pod, over the network, on the wrong machine. vibepod
therefore writes an instruction file into the pod — linked at `/CLAUDE.md` and
`/AGENTS.md`, which all three agents read from the working directory upwards —
naming the machines, the directories and the three commands that matter. It is
regenerated whenever the mounts change, and `vp brief` prints what the agent was
actually told.

## Seeing where things went

`vp tree` is the view nothing else can produce — `pstree` stops at the machine
boundary, and here the machine is a column:

```console
$ vp tree work
work · running 3m12s · new sessions open on pod

mounts
├─ /remote/vast0/…/proj      ← gpu03:/remote/vast0/…/proj   sshfs  rw
└─ ~/Git/notes               ← local ~/Git/notes            bind   rw

sessions
├─ session 1  on pod  (shell)
│  └─ claude                              pod      3m12s ●
└─ session 2  on gpu03  (shell)
   └─ python train.py                     gpu03    1m04s ●

(47 completed — `vp tree --all` to expand)
```

For agents and scripts: `vp tree --json` (frugal by default — live work in full,
finished work as a count, because context is a budget too) and `vp log -f --json`,
which streams the daemon's own event stream as NDJSON with a `"v"` field.

An agent inside a pod reaches the daemon through a socket bound into the
namespace, and what it may do is decided by *which socket* it can reach: reading
yes; moving its own session yes, since that is how work reaches another machine
at all; `up` and `down` no; mounting only within the allowlist.

## Three honest notes

**Exit status.** The log shows a status for commands vibepod was the parent of —
every dispatched one. For a command the pod's `$SHELL` merely recorded, it knows
the command ended but not what it returned, so the field is absent rather than
zero.

**Waiting.** Creating a pod connects to every machine it mounts from, and each
gets ten seconds to prove it exists. That wait reports itself — `connecting to
gpu03… connected (240ms)` — and a failure names the step and says what to check
rather than printing ssh's own text at you.

**Guardrails.** There are none, by choice. vibepod routes and records; it does not
judge. Your agent already gates commands, and pattern-matching shell strings for
`rm -rf` is leaky in both directions. The log is the answer. Mounting is the one
exception, and for a different reason: it opens a network path out of a sandbox
whose purpose was containment.

## Reaching data through this machine

A node pod mounts what it needs itself, from the machine that owns it. When it
cannot — node-to-node ssh is firewalled on many clusters — `via: auto` (the default)
notices and relays through this machine instead, and says why in `vp log`. A
directory on *this* machine reaches a node only if its mount lists that node:

```yaml
  - local: ~/Git/proj
    expose_to: [gpu05]         # in gpu05's pod at the same path: edit here, run there
```

The relay is `rclone serve sftp` on this machine's loopback, reached through a
reverse forward on the ssh connection vibepod already holds, and authenticated by a
key made for that one tunnel. It needs rclone here.

Also available: `ports: [gpu03:8888]` (a remote port on localhost here),
`forward_credentials: true` per host (lend that machine your ssh agent — off by
default, and it is authority lent for as long as the connection lives),
`toolbin: true` per host (copy a missing static tool there), and `mode: sync` (a
local copy instead of a network mount, kept the same around each command).

## Layout

```
cmd/vibepod     the client, the cockpit, the daemon, vpinit and vpnode
cmd/vpsh        the pod's $SHELL: records a command line, then runs it
internal/pod    namespace construction, and vpinit
internal/node   a pod on a machine that is not yours
internal/daemon registry, backends, dispatch, sessions, mounts, the agent brief
internal/route  what a directory is called on a given machine
internal/remote ssh multiplexing, dispatch, live shells, the pushed footprint
internal/fs     remote mounts (rclone, sshfs)
internal/sys    capabilities, mounts, ptys
test            end-to-end: real namespaces, a real sshd, real terminals
```

`DESIGN.md` is the argument; `PLAN.md` records what was measured before any of it
was built, and the one design decision the measurements overturned.
