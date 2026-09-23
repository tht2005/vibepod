# vibepod

Run your code agent on your own machine, against code that lives on another
one, and let its commands run where the code is.

Working on a remote server over SSH usually means one of two bad trades:
install Claude Code, Codex or OpenCode on every remote and copy your API keys,
MCP config and skills to each — or work locally and lose the remote's
toolchain, data and services.

vibepod inverts it. A *pod* is a local namespace that composes three things:

| | |
|---|---|
| **Composition** | local and remote directories in one filesystem view |
| **Identity** | your agent config, skills and credentials, bound from the host — **never transmitted to a remote** |
| **Execution** | each command runs on the machine that owns its directory |

The first two are plumbing. The third is the point.

```console
$ vpctl new work
vibepod · work
  /srv/api → prod

/srv/api [auto] ❯ claude
```

Inside that pod the agent sees one filesystem. When it runs `cargo test` in
`/srv/api`, that runs on `prod`, against prod's disk, with prod's toolchain —
and the output streams back. When it runs `git commit` in `~/Git/notes`, that
runs here. Nobody had to tell it which.

## How it knows

Two mechanisms, both below the agent:

**An exec gate.** `vpinit` installs a seccomp filter that traps `execve` as a
user notification, so the daemon sees *every* program launch in the pod —
including statically linked and agent-bundled binaries that no `$PATH` shim
would catch.

**Lazy bind-shims.** seccomp notification is a gate, not a rewriter, so the
redirect uses the one thing it does give: the syscall is frozen while the
daemon decides. The daemon stashes the original binary, bind-mounts `vpsh` over
its path, and replies *continue* — the kernel resolves the path after that, and
finds the shim. First exec of a binary costs one mount; the rest are free.

The shell deliberately stays local. Agents wrap commands in a generated shell
script that sources an environment snapshot and records the new working
directory in a local temp file; shipping that string to another machine breaks
the environment silently and writes the cwd file on the wrong host. Intercept
one level down, at the program, and `cd` keeps working.

## What never leaves your machine

- **API keys and agent credentials.** Bound into the pod; never transmitted.
- **SSH private keys.** The pod cannot see them at all — the daemon runs
  outside every namespace and owns all the connections.
- **Anything you did not name.** Only paths in `host_access:` are bound in, so
  an agent working on a client's remote code cannot read `~/.aws` or a sibling
  project.

What does cross the wire: the command strings you route, and remote source code
coming *to* you (and so to the model). That is inherent to the premise.

Processes in a pod hold **no capabilities at all** — they cannot mount,
unmount, or remove a shim. `vpinit` keeps `CAP_SYS_ADMIN` for the pod's life
because lazy shims need it, and hands out none of it. That said: a namespace
contains accidents, not adversaries.

## Install

Needs Linux 5.7+ (seccomp user notification), unprivileged user namespaces, and
`ssh`. For remote mounts, `rclone` (preferred) or `sshfs`. No root, ever.

```console
$ make install          # ~/.local/bin/{vibepod,vpsh,vpctl}
$ vpctl doctor
```

## Configure

`vibepod.yaml`, next to your project. Hosts are **ssh_config aliases**, so
ProxyJump, keys and ports are inherited rather than reimplemented.

```yaml
pod: work

mounts:
  - remote: prod:/srv/api      # appears at /srv/api; commands here run on prod
  - local: ~/Git/notes         # commands here run in the pod
  - remote: prod:/var/log/api
    readonly: true

host_access:                   # bound from the host, never transmitted
  - ~/.claude
  - ~/.claude.json

exec:
  default: pod                 # for a directory matching no mount
```

A remote directory mounts at **its own absolute path**. That is not cosmetic:
arguments forward verbatim, and remote tool output stays openable — a compiler
error naming `/srv/api/src/db.rs:14` is a path the agent can actually open.
`vpctl up` refuses a mount that would shadow a system path.

## Use

```
vpctl new [name]              create a pod and open the console
vpctl up [name]               create one detached, for scripts
vpctl run [name] -- cmd...    run a command in a pod
vpctl run claude              launch an agent in it
vpctl shell [name]            another terminal on a running pod
vpctl attach [name] [sess]    return to a session you detached from
vpctl ps                      running pods
vpctl tree [name]             mounts and live execs, in one view
vpctl log [-f] [name]         every command and the machine it ran on
vpctl use <host|auto>         send this session's commands to one machine
vpctl down [name]             stop it and release its mounts
vpctl doctor                  check this machine can host a pod
```

`Ctrl-\` detaches and leaves the session running. Pods are named and outlive
the terminal that made them, like containers.

### Seeing where things went

`vpctl tree` is the view nothing else can produce — `pstree` stops at the
machine boundary, and here the machine is a column:

```console
$ vpctl tree work
work · running 3m12s · exec: auto

mounts
├─ /srv/api        ← prod:/srv/api      sshfs  rw
│     exec_on → prod
└─ ~/Git/notes     ← ~/Git/notes        bind   rw

exec
└─ session 1
   └─ claude                            pod      3m12s ●
      └─ cargo test                     prod      12.4s ●

(47 completed — vpctl tree --all to expand)
```

The two halves are one view because the mount half explains the exec half.

For agents and scripts: `vpctl tree --json` (frugal by default — live work in
full, finished work as a count, because context is a budget too) and
`vpctl tree -f` or `vpctl log -f --json`, which stream the daemon's own event
stream as NDJSON with a `"v"` field.

An agent inside a pod reaches the daemon through a socket bound into the
namespace, and what it may do is decided by *which socket* it can reach:
`tree` and `log` yes; `up` and `down` no; `use` only from something holding a
terminal — otherwise an agent could re-route itself to a machine its directory
would never have chosen.

## Two honest notes

**Exit status.** The log shows a status only for commands vibepod was the
parent of — routed ones. For a pod-local exec it knows the command ended but
not what it returned, so the field is absent rather than zero.

**Guardrails.** There are none, by choice. vibepod routes and records; it does
not judge. Your agent already gates commands, and pattern-matching shell
strings for `rm -rf` is leaky in both directions. The log is the answer.

## Not in v1

Credential proxying for routed `git push`, on-demand tool push (`toolbin:`),
port forwards, reverse mounts (`expose_to:`, and the `exec_on:` that depends on
them), `sync` mode, and `vpctl tree -x` to expand a remote subtree. See
`DESIGN.md` §13.

## Layout

```
cmd/vibepod     vpctl, the daemon, and vpinit (one binary, three names)
cmd/vpsh        the shim
internal/pod    namespace construction, and vpinit
internal/daemon registry, exec gate, routing, sessions
internal/route  cwd → machine
internal/remote ssh multiplexing and remote exec
internal/fs     remote mounts (rclone, sshfs)
internal/sys    seccomp, capabilities, mounts, ptys
test            end-to-end: real namespaces, a real sshd, real terminals
```

`DESIGN.md` is the argument; `PLAN.md` records what was measured before any of
it was built, and the one design decision the measurements overturned.
