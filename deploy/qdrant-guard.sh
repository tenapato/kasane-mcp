#!/usr/bin/env bash
# Crash-loop guard for the Qdrant container (see docs/operations.md).
#
# Docker cannot cap restarts for `unless-stopped`, and `on-failure:N` does not
# start the container again after a daemon or host restart. This wrapper keeps
# `unless-stopped` (Qdrant comes back after a reboot) but stops trying after
# QDRANT_GUARD_MAX_FAILURES consecutive failed runs, so corrupted storage cannot
# turn into an endless restart loop that hammers the disk.
#
# A failed run is a non-zero exit that was not requested by `docker stop` and
# happened within QDRANT_GUARD_STABLE_SECONDS of starting. A clean stop or a
# run that stayed up long enough resets the count. The counter lives on the
# storage volume, so wiping storage during recovery also clears it.
set -u

counter=${QDRANT_GUARD_FILE:-/qdrant/storage/.kasane-qdrant-failures}
max=${QDRANT_GUARD_MAX_FAILURES:-5}
stable=${QDRANT_GUARD_STABLE_SECONDS:-300}

park() {
	echo "kasane qdrant-guard: $1" >&2
	echo "kasane qdrant-guard: Qdrant is NOT running. Kasane keeps serving from PostgreSQL. Follow docs/operations.md, then remove $counter and restart this container." >&2
	trap 'exit 0' TERM INT
	sleep infinity &
	wait
	exit 0
}

failures=$(cat "$counter" 2>/dev/null)
[[ $failures =~ ^[0-9]+$ ]] || failures=0
if ((failures >= max)); then
	park "Qdrant failed $failures times in a row; refusing to start it again to protect the disk."
fi

stopping=0
trap 'stopping=1; kill -TERM "$pid" 2>/dev/null' TERM
trap 'stopping=1; kill -INT "$pid" 2>/dev/null' INT
started=$SECONDS
./entrypoint.sh "$@" &
pid=$!
wait "$pid"
code=$?
# A trapped signal interrupts wait; keep waiting for the real exit status.
while kill -0 "$pid" 2>/dev/null; do
	wait "$pid"
	code=$?
done

if ((stopping || code == 0 || SECONDS - started >= stable)); then
	next=0
else
	next=$((failures + 1))
	echo "kasane qdrant-guard: Qdrant exited with code $code after $((SECONDS - started))s (consecutive failure $next of $max)." >&2
fi
if ! echo "$next" >"$counter"; then
	park "cannot record the failure count in $counter (storage may be read-only or failing)."
fi
exit "$code"
