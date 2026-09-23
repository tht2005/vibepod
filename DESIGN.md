# vibepod — design

> Status: design in progress. Decisions marked **[open]** still need a call.

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

**Inside the pod, `/bin/sh` and `/bin/bash` are `vpsh`, a dispatcher.**

The pod is a mount namespace, so a fake shell can be bind-mounted over the real one
*inside the pod only*, without touching the host.

```
agent runs:  bash -c "cargo test"        (cwd = /srv/api)
vpsh:        → vibepod: {pod, cwd, argv, env, tty?}
vibepod:     /srv/api is mounted from prod → warm ControlMaster
             ssh prod 'cd /srv/api && exec bash -c "cargo test"'
             stream stdout/stderr/exit back
```

Why this interception point:

- **Universal by construction.** Claude, Codex, OpenCode, or a shell script — anything
  that shells out gets routed. No per-agent plugin. The agent never knows.
- **No parsing.** The `-c` string is forwarded verbatim. Pipelines, heredocs,
  redirections, `&&` stay bash's problem.
- **One policy file.** Routing lives in the daemon, not scattered through shims.

Known limitation: a pipeline picks **one** side. `cat local.txt | remote-tool` runs
entirely on one machine. Documented, not fixed.

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
│  │   vpinit (PID 1) ─────────────┤  holds ns, reaps, signals   │
│  │   /bin/sh → vpsh ─────────────┘  every command routes here  │
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
| `vpinit` | PID 1 inside each pod. Holds the mount namespace open, reaps zombies, forwards signals |
| `vpsh` | the `/bin/sh` shim. Forwards `(cwd, argv, fds, tty)` to the daemon, proxies exit code |

`vpinit` exists because **a mount namespace only survives while a process is inside it.**
Without it, detaching would destroy the pod.

## 5. Routing

Default: **cwd decides the machine.**

| cwd | runs on |
|---|---|
| inside a remote mount | that host, at the translated path |
| inside a local bind mount | in the pod |
| anywhere else in the pod | in the pod |

Overrides: `@prod cmd`, `@local cmd`, `@pod cmd`.

Pod-internal paths are permanently exempt, so MCP servers and agent-internal helpers
always run locally and never get shipped to a remote.

### Path identity  **[open — see §12]**

Proposed: a remote dir mounts at **its own absolute path** by default.
`prod:/srv/api` appears in the pod as `/srv/api`, not `/work/api`.

This eliminates path translation entirely. Without it, `cat /work/api/x` forwarded to
prod fails, because prod has no `/work/api` — and rewriting path-like arguments is
fragile guesswork. With it, every path is valid verbatim on both sides.

`at:` remains available for the rare case where two hosts collide on a path.

## 6. Filesystem modes

`mode:` is per-mount, so the strategy can change without changing the config shape.

| mode | behaviour | good for |
|---|---|---|
| `fuse` (default) | sshfs with aggressive caching. Remote is the single source of truth, zero drift | most repos, low-RTT links |
| `sync` | bidirectional sync to a local scratch dir. Local-disk read speed | large repos, high-RTT links |
| `bind` | local directory, no network | local dirs |

**The known risk:** agents are read-storms — glob, grep, read 40 files. At 30ms RTT,
sshfs turns a 2-second `rg` into 40 seconds. This is the biggest UX risk in the design
and the reason `sync` exists as an escape hatch.

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

**Honest limit.** bwrap is a namespace, not a security boundary against a determined
attacker. It contains accidents, not adversaries.

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
  buildbox:
    forward_credentials: true

mounts:
  - remote: prod:/srv/api          # → /srv/api in pod
    mode: fuse
  - local: ~/Git/notes             # → ~/Git/notes in pod
  - remote: prod:/var/log/api
    mode: fuse
    readonly: true

host_access:                       # bound from host into pod
  - ~/.claude
  - ~/.config/opencode

ports:
  - prod:3000                      # auto -L forward

exec:
  default: pod                     # when cwd matches no mount
```

## 9. Command surface

```
vpctl up [pod]              create + start; bare form reads ./vibepod.yaml
vpctl ps                    list pods, mounts, routes, health
vpctl shell [pod]           interactive shell inside the pod
vpctl run claude [pod]      launch an agent inside the pod
vpctl attach [pod]          reattach to a detached session
vpctl exec @prod -- cmd     one-off, explicit target
vpctl mounts [pod]          show the route table
vpctl logs [-f] [pod]       audit log / session output
vpctl down [pod]            stop, unmount, disconnect
vpctl doctor                check bwrap, sshfs, ssh reachability, remote toolchains
```

Pods are **named** and globally listable, but a bare `vpctl up` in a project directory
takes its name from `./vibepod.yaml`. Explicit when you want it, zero-argument when
you're in a project.

## 10. Detach & reattach

`vibepod` owns the PTY and keeps a scrollback ring buffer (dtach/abduco-style, built in —
no tmux dependency). Detaching leaves the agent running; `vpctl attach` reconnects and
replays the buffer. Killing the pod kills everything inside it.

## 11. Decisions locked

| Decision | Choice | Why |
|---|---|---|
| Pod backend | bubblewrap namespace | ~10ms start, no image, reuses host binaries, and the only way to bind a fake `/bin/sh` without touching the host |
| Topology | central `vibepod` daemon + per-pod `vpinit` | one socket, one audit log, shared ssh muxes; `vpinit` covers the namespace-lifetime requirement |
| Privilege | rootless, auto-spawned | credentials are user-owned; root buys nothing and costs the security story |
| Exec routing | cwd-inferred + `@host` override | no invisible mode state; agents get it right with zero prompting |
| Interception | `/bin/sh` → `vpsh` | agent-agnostic, no command parsing |
| Remote FS | `fuse` default, `mode:` per mount | ship fast, escape hatch for latency without a config break |
| Detach | dtach-style, built into the daemon | no tmux dependency, no prefix-key collisions |
| Credentials | per-command reverse proxy, per-host opt-in | nothing stored remotely; trust decided per machine |
| Remote footprint | push on demand, prompt first, `toolbin:` pre-authorizes | no speculative installs, no mid-run interruptions once trusted |
| Pod identity | named, resolved from cwd's `vibepod.yaml` | docker-like when explicit, zero-argument in a project |
| Hosts | ssh_config aliases | inherits ProxyJump/keys/ports for free |
| Language | Go | os/exec, PTY, sockets, goroutine stream-plumbing are first-class; ~3ms vpsh startup is negligible against RTT |

## 12. Open questions

1. **Path identity** (§5) — mount remote dirs at their own absolute path by default?
2. **`host_access` model** — explicit allowlist, or bind `$HOME` minus a denylist?
3. **v1 scope** — how much of this ships in the first working version?
4. Link-drop behaviour: reconnect transparently, or fail in-flight commands loudly?
5. Multiple remotes in one pod — supported by the model, but is it a v1 goal?
6. `sync` mode implementation: rsync loop, or embed a mutagen-style watcher?

## 13. Milestones

- **M0 — the trick works.** Pod with local binds only, `vpsh` routing everything back to
  the pod. Proves namespace + shim + fd plumbing end to end.
- **M1 — one remote.** Daemon, sshfs mount, cwd routing to a single host, warm
  ControlMaster. This is the first genuinely useful version.
- **M2 — lifecycle.** `vpinit`, detach/attach, PTY buffer, `ps`/`down`.
- **M3 — real work.** Credential proxy, toolbin prompts, port forwards.
- **M4 — polish.** Multi-host, audit log, `doctor`, `sync` mode.
