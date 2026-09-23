# vibepod demo

A sandbox with two "remote machines", **alpha** and **beta**, so you can try
every part of vibepod without a cluster. Both are this machine, reached through a
private sshd on port 2299. alpha holds a small "dataset" and a tool your laptop
does not have (`gpu-smi`); beta holds nothing — it plays the GPU box.

It runs its own daemon, so it will not disturb a vibepod you already use.

Needs: `sshd` and `sshfs` (or `rclone`) installed. No root.

## 0. Set up

```sh
cd ~/Git/vibepod/examples/demo
./setup.sh
. ~/.cache/vibepod-demo/env.sh     # puts ./bin on PATH, points at the sandbox,
                                    # and cd's into the demo project
```

Run every step below in that same terminal (or source `env.sh` again in a new one).

## 1. Start the pod

```sh
vibepod up
```

You should see it connect to alpha and mount its data, each step timed.

## 2. Look around

```sh
vp hosts        # pod (here), alpha (mounted), beta (not mounted)
vp tree         # the mounts, and the sessions (none yet)
vp brief        # the instructions an agent in this pod is given
```

## 3. One command on another machine

```sh
vp run -- sh -c 'echo "here: ${SSH_CONNECTION:-no ssh}"'
vp @alpha sh -c 'echo "there: ${SSH_CONNECTION:+over ssh}"'
vp run -- gpu-smi                   # exists only on alpha; a wrapper sends it there
```

The machine is chosen, never guessed: a plain command runs here, `@alpha` sends
it there, and `gpu-smi` works by name because the config lists it under
`remote_tools`.

## 4. The same path on both machines

```sh
vp run -- sh -c "cd $DEMO/alpha-data && ls && vp @alpha pwd"
```

alpha's directory is mounted at the path alpha itself uses, so `pwd` over there
prints exactly the path you are in here. Tracebacks from alpha are paths you can
open.

## 5. A real shell over there

```sh
vp shell -on alpha -C $DEMO/alpha-data
```

You are now in a login shell *on alpha*. Try:

```sh
echo $SSH_CONNECTION      # set: you are over ssh
cd .. && pwd              # cd sticks, because it is cd
export X=1; echo $X       # so do variables
```

Press **Ctrl-\\** to detach. The shell keeps running. `vp ps` lists it;
`vp attach demo <session>` brings it back. Type `exit` to end it.

## 6. Move a session between machines

```sh
vp shell                  # a shell in the pod, on this machine
X=here                    # remember something
vp use alpha              # this terminal is now a shell on alpha
echo ${SSH_CONNECTION:+on alpha}
```

`vp` is not installed on alpha (nothing is), so the way back is from outside. In
a **second terminal** (after `. ~/.cache/vibepod-demo/env.sh`):

```sh
vp ps                     # find the session number
vp use -s <N> pod
```

Back in the first terminal: `echo $X` still says `here` — the shell you left
kept its state. `exit` when done.

## 7. Add a machine without restarting

```sh
vp mount beta:$DEMO/beta-data
vp run -- cat $DEMO/beta-data/results.txt
vp hosts                  # beta is mounted now
vp unmount @beta          # and gone again
```

No `down`, no `up`, no lost sessions. `vp save` would write the live state back
to `vibepod.yaml` (it asks before overwriting).

## 8. Compute on one machine, data on another

beta has no data of its own. Send it a command in alpha's directory:

```sh
vp run -- sh -c "cd $DEMO/alpha-data && vp @beta cat shard-1.txt"
```

**Refused** — on a real cluster the same path on beta could hold *different
bytes*, and running there would silently use them. Give beta a pod that
reproduces the tree:

```sh
vp node add beta          # copies one binary to ~/.vp/bin on beta; nothing else
vp run -- sh -c "cd $DEMO/alpha-data && vp @beta cat shard-1.txt && vp @beta pwd"
vp node                   # what beta holds, and whether it is behind
```

Now beta reads alpha's data at the same path. Add a mount while beta's pod
exists and it follows:

```sh
vp mount beta:$DEMO/beta-data
vp node                   # beta's pod holds it too, at a new generation
```

beta's *own* directory, added after its pod was built, shows as "through a local
FUSE hop" — a running namespace cannot be handed a new bind. `vp node drop beta`
then `vp node add beta` rebuilds the pod and it becomes a plain bind.

Directories on *this* machine (`project/`) reach a node only if the mount says
`expose_to: [beta]` (which needs rclone here, to serve it). Without that, a command
sent to beta from one of them runs in beta's home directory, and `vp log` says so
once.

## 9. The cockpit

```sh
vibepod
```

Arrows or `j`/`k` move, **Tab** switches to the sessions list, **Enter** hands
your terminal to the selected session (Ctrl-\\ comes back), `b` moves a
session's machine, `m` mounts, `u` unmounts, `/` filters the activity, `q`
quits. Quitting leaves the pod running.

## 10. What happened, and where

```sh
vp run -- sh -c 'vp @alpha sh -c "exit 3"'   # something that fails
vp log                    # every command, the machine it ran on, its exit code
vp tree --failed --since 10m
```

And to see what a remote command is doing: in a **second terminal**, run
`vp run -- sh -c 'vp @alpha sh -c "sleep 30; true"'`, then in the first:

```sh
vp tree -x --running      # the remote child shows up, marked (polled)
```

## 11. (Optional) An agent in the pod

```sh
vp run claude             # or codex / opencode
```

It reads `/CLAUDE.md` inside the pod, which tells it about `vp @alpha` and
`vp use`. Ask it to "run gpu-smi on alpha and summarise". Watch `vp log -f` in a
second terminal.

## Clean up

```sh
cd ~/Git/vibepod/examples/demo && ./teardown.sh
```

Stops the pod, the demo daemon and the sshd, and removes everything under
`~/.cache/vibepod-demo`. Step 8 leaves one file you may want to delete:
`rm -rf ~/.vp`.

## If something goes wrong

- `port 2299 is already in use` → `PORT=2300 ./setup.sh`
- anything else → the daemon's log: `$DEMO/run/daemon.log`, and the sandbox
  sshd's: `$DEMO/sshd/sshd.log`
