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

So vibepod does not translate paths. It reproduces them: `vp node add gpu05`
builds a pod *on gpu05* holding the same composed tree at the same absolute
paths. What crosses is one binary in `~/.vp/bin` and a description of the mounts.
No credentials, no agent, nothing installed outside that directory, and `vp node
drop` removes what it made.

The node keeps its **own** `/usr`, `/etc` and `/opt`, because that difference is
the reason to dispatch there at all — a minimal root on a GPU box hides the GPUs
and reads as "ROCm is broken". A mount can say what it needs of whoever runs it:

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
vp shell [pod] [-on machine]  another terminal on a running pod
vp attach [pod] [session]     return to a session you detached from
vp run [pod] -- cmd ...       one command in a pod, creating it if needed
vp brief                      the instructions the agent in this pod was given
```

`Ctrl-\` detaches and leaves the session running. Pods are named and outlive the
terminal that made them, like containers. A terminal that disappears without
detaching is also a detach: an agent halfway through something is not killed by
the disappearance of the thing that was watching it.

## The cockpit

`vibepod` with no arguments:

```
vibepod · work
MACHINES                                │ACTIVITY
● pod*        ~/Git/notes                │14:02:11 gpu03  python train.py      ●
● gpu03       /remote/vast0/…/proj       │14:01:40 pod    rg TODO           ✓ .3s
○ gpu05       vp mount gpu05:/path       │13:58:02 gpu03  rocm-smi          ✓
                                         │
SESSIONS                                 │
1   claude     ▸ pod                     │
2   shell      ▸ gpu03                   │
                                         │
MOUNTS                                   │
  /remote/vast0/…/proj    gpu03:/remote… │
─────────────────────────────────────────┴──────────────────────────────────
 backend gpu03 │ m mount  u unmount  b backend  ⏎ attach  ⇥ pane  / filter  q quit
```

`⏎` does not draw a terminal inside a pane. It **leaves the alt-screen, hands the
raw terminal to the session, and redraws when you come back** — what lazygit does
with `$EDITOR`. Running an agent inside a homemade multiplexer means nested
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

## Not built yet

`via: relay` for a node with no path to the data machine, the forwarded-agent
credential proxy, `toolbin:` pushes, port forwards, reverse mounts (`expose_to:`),
`sync` mode, and the control plane that would let `vp mount` converge a pod that
already has node pods — today that combination is a visible refusal naming the
mount, and `down`/`up` replicates the new tree. `DESIGN.md` §13 has the list.

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
