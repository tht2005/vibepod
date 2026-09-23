#!/bin/sh
# Builds a sandbox with two "remote machines" — alpha and beta — so every part of
# vibepod can be tried without a real cluster. Both are this machine, reached
# through a private sshd on port 2299 that nothing else uses. It lives under /tmp
# because OpenSSH 10's per-connection child cannot read through a 0700 home
# directory, and resets every connection if its keys are in one. Nothing outside
# $DEMO is touched, except that a node pod (step 6) puts the vibepod binary in
# ~/.vp/bin, because alpha and beta *are* this machine.
set -e
REPO=$(cd "$(dirname "$0")/../.." && pwd)
DEMO=${DEMO:-$HOME/.cache/vibepod-demo}
PORT=${PORT:-2299}

command -v sshd >/dev/null || [ -x /usr/bin/sshd ] || { echo "needs sshd installed"; exit 1; }
command -v sshfs >/dev/null || command -v rclone >/dev/null || { echo "needs sshfs or rclone"; exit 1; }

echo "building vibepod…"
make -C "$REPO" build >/dev/null

rm -rf "$DEMO"
mkdir -p "$DEMO"/sshd/bin "$DEMO"/alpha-data "$DEMO"/beta-data "$DEMO"/project "$DEMO"/run
cd "$DEMO"

# The "remotes'" own files.
echo "dataset shard 1 — lives on alpha" > alpha-data/shard-1.txt
echo "dataset shard 2 — lives on alpha" > alpha-data/shard-2.txt
echo "results from beta" > beta-data/results.txt
echo "my local notes" > project/notes.md

# A tool that exists only on the "remotes", the way rocm-smi exists only on gpu03.
cat > sshd/bin/gpu-smi <<'T'
#!/bin/sh
echo "gpu-smi on $(uname -n): 8x MI250 (pretend) — you are over ssh: ${SSH_CONNECTION:+yes}"
T
chmod 755 sshd/bin/gpu-smi

ssh-keygen -q -t ed25519 -f sshd/hostkey -N ''
ssh-keygen -q -t ed25519 -f sshd/id -N ''
cp sshd/id.pub sshd/authorized_keys && chmod 600 sshd/authorized_keys
SFTP=/usr/lib/ssh/sftp-server
for c in /usr/lib/ssh/sftp-server /usr/libexec/sftp-server /usr/lib/openssh/sftp-server; do
  [ -x "$c" ] && SFTP=$c && break
done
cat > sshd/sshd_config <<C
Port $PORT
ListenAddress 127.0.0.1
HostKey $DEMO/sshd/hostkey
AuthorizedKeysFile $DEMO/sshd/authorized_keys
PidFile $DEMO/sshd/sshd.real.pid
StrictModes no
UsePAM no
PrintMotd no
Subsystem sftp $SFTP
SetEnv PATH=$DEMO/sshd/bin:/usr/local/bin:/usr/bin:/bin
C
for h in alpha beta; do cat >> sshd/ssh_config <<C
Host $h
  HostName 127.0.0.1
  Port $PORT
  User $USER
  IdentityFile $DEMO/sshd/id
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
C
done

SSHD=$(command -v sshd || echo /usr/bin/sshd)
# OpenSSH 9.8+ penalises a source address that drops connections, and a sandbox on
# loopback that is being started and probed would lock itself out. Older sshd does
# not know the option and refuses the whole file, hence the check.
echo "PerSourcePenalties no" >> sshd/sshd_config
"$SSHD" -t -f "$DEMO/sshd/sshd_config" 2>/dev/null ||
  sed -i '/PerSourcePenalties/d' "$DEMO/sshd/sshd_config"
# The config path must be absolute: sshd changes to / and re-reads it for every
# connection, so a relative path works at startup and then resets each connection
# before authentication, with nothing in the log to say why.
( "$SSHD" -D -f "$DEMO/sshd/sshd_config" -E "$DEMO/sshd/sshd.log" \
    </dev/null >/dev/null 2>&1 &
  echo $! > "$DEMO/sshd/sshd.pid" )
for i in $(seq 1 50); do
  ssh -F sshd/ssh_config -o BatchMode=yes -o ConnectTimeout=2 alpha true 2>/dev/null && break
  [ "$i" = 50 ] && { echo "sshd did not come up; see $DEMO/sshd/sshd.log"; exit 1; }
  sleep 0.1
done

cat > project/vibepod.yaml <<Y
pod: demo

hosts:
  beta: {}                   # a machine with no data of its own: pure compute

mounts:
  - remote: alpha:$DEMO/alpha-data   # alpha's data, at the same path in the pod
  - local: $DEMO/project             # this machine's files

remote_tools:
  alpha: [gpu-smi]           # exists only on the remotes; wrapped in /vp/bin

can_mount: [alpha, beta]     # what an agent inside the pod may mount

exec:
  default: pod               # new sessions start here
Y

# A separate daemon, so this cannot disturb a vibepod you already run.
cat > env.sh <<E
export PATH="$REPO/bin:\$PATH"
export VIBEPOD_RUNDIR="$DEMO/run"
export VIBEPOD_SSH_CONFIG="$DEMO/sshd/ssh_config"
export VIBEPOD_NODE_SSH_CONFIG="$DEMO/sshd/ssh_config"
export DEMO="$DEMO"
cd "$DEMO/project"
E

echo
echo "ready. now run:"
echo
echo "  . $DEMO/env.sh"
