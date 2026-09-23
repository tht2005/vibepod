# gpulab: gpu03 + aiotlab_3gpus_aiotlab

One pod, two real GPU machines:

| machine | hardware | its directories in the pod |
|---|---|---|
| `gpu03` | AMD MI250, ROCm | `/remote/vast0/duongnguyen/gpu-intern-26`, `/remote/vast0/duongnguyen/models` (ro) |
| `aiotlab_3gpus_aiotlab` | 3× NVIDIA A30 | `/home/aiotlab/apps`, `/home/aiotlab/data` (ro) |

Your agent and its credentials stay on this machine. Nothing is installed on
either server unless you run `vp node add` (step 6), and then only one binary in
`~/.vp/bin`.

## 0. Before the first run

```sh
cd ~/Git/vibepod && make install     # puts vibepod, vp, vpsh in ~/.local/bin
vibepod down <your-old-pods>         # the v1 daemon is still running; stop its
                                     # pods and the new one starts on next use
```

Or, to try it without touching your installed copy, use a private daemon:

```sh
export PATH=~/Git/vibepod/bin:$PATH VIBEPOD_RUNDIR=~/.cache/vibepod-gpulab
```

## 1. Up

```sh
cd ~/Git/vibepod/examples/gpulab
vibepod up
vp hosts
```

It connects to both machines and mounts four directories, each step timed.

## 2. Each machine's own tools, by name

```sh
vp run -- rocm-smi --showproductname           # runs on gpu03
vp run -- nvidia-smi                           # runs on aiotlab
```

Neither tool exists here. `remote_tools:` gives each a wrapper that sends it to
the machine that has it.

## 3. One command, a machine of your choice

```sh
vp @gpu03 uname -n                             # mv-mi250-03
vp @aiotlab_3gpus_aiotlab uname -n             # gpus-Super-Server
```

## 4. Work on gpu03's files, on gpu03

```sh
vp run -- sh -c 'cd /remote/vast0/duongnguyen/gpu-intern-26/hip-matrix-core && ls && vp @gpu03 hipcc --version'
```

The directory is the same path here and on gpu03, so `cd` then `vp @gpu03 …`
compiles in the right place with gpu03's toolchain. For a real shell there:

```sh
vp shell -on gpu03 -C /remote/vast0/duongnguyen/gpu-intern-26
```

`cd` sticks, `make` runs on the MI250s, **Ctrl-\\** detaches and leaves it
running, `vp attach gpulab <n>` returns.

## 5. aiotlab's data, readable from here

```sh
vp run -- ls /home/aiotlab/data
```

Read-only in this config, since it is a dataset.

## 6. gpu03's code on aiotlab's GPUs

This is the case the design was built for, so try it:

```sh
vp run -- sh -c 'cd /remote/vast0/duongnguyen/gpu-intern-26 && vp @aiotlab_3gpus_aiotlab ls'
```

**Refused.** aiotlab has no `/remote/vast0/duongnguyen/…` of its own, and if it
did it would be different files. To have aiotlab hold this pod's tree at the same
paths:

```sh
vp node add aiotlab_3gpus_aiotlab    # asks nothing more: typing it is the consent.
                                     # Copies vibepod to aiotlab:~/.vp/bin, nothing else.
vp run -- sh -c 'cd /remote/vast0/duongnguyen/gpu-intern-26 && vp @aiotlab_3gpus_aiotlab ls'
vp node                              # what aiotlab holds
```

aiotlab mounts gpu03's directory *itself* if it can ssh to gpu03 with its own
keys. If it cannot, vibepod relays gpu03's data through your laptop instead and
says so in `vp log` — which needs rclone on your laptop. Two alternatives:

- lend aiotlab your ssh agent for this pod, so it can reach gpu03 directly:
  `hosts: {aiotlab_3gpus_aiotlab: {forward_credentials: true}}`. The key stays on
  your laptop; aiotlab can use it while the connection lives.
- force a route with `via: direct` or `via: relay` on the mount.

`vp node drop aiotlab_3gpus_aiotlab` removes the pod and its state from aiotlab.

## 7. The cockpit, and the log

```sh
vibepod                  # machines, sessions, activity; Enter attaches, q quits
vp log                   # every command, and the machine it ran on
```

## 8. An agent

```sh
vp run claude
```

Ask it something that needs both machines — "compare the GPUs on gpu03 and on
aiotlab". It knows how from `/CLAUDE.md` inside the pod (`vp brief` shows it).

## Done

```sh
vibepod down gpulab      # unmounts everything, stops any node pods it started
```
