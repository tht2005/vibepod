#!/bin/sh
# Stops everything setup.sh started and removes what it made.
DEMO=${DEMO:-$HOME/.cache/vibepod-demo}
REPO=$(cd "$(dirname "$0")/../.." && pwd)
export VIBEPOD_RUNDIR="$DEMO/run" VIBEPOD_SSH_CONFIG="$DEMO/sshd/ssh_config"
"$REPO/bin/vibepod" down demo 2>/dev/null
# The daemon for this run directory, and the sshd.
for p in $(pgrep -f "vibepod daemon"); do
  grep -qz "VIBEPOD_RUNDIR=$DEMO/run" /proc/$p/environ 2>/dev/null && kill $p
done
[ -f "$DEMO/sshd/sshd.pid" ] && kill "$(cat "$DEMO/sshd/sshd.pid")" 2>/dev/null
sleep 1
awk -v d="$DEMO" '$5 ~ d {print $5}' /proc/self/mountinfo | sort -r | while read m; do
  fusermount3 -u -z "$m" 2>/dev/null || umount "$m" 2>/dev/null
done
rm -rf "$DEMO" "$HOME"/.vp/run/demo@*
echo "demo removed. (~/.vp/bin/vibepod, if step 8 put it there, is yours to rm.)"
