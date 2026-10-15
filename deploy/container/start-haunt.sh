#!/bin/sh
set -eu
if [ "${HAUNT_CONTAINER_LOCAL_DATABASE:-0}" = 1 ]; then
    for _ in $(seq 1 60); do
        if pg_isready -q -h /run/postgresql -U haunt -d haunt; then break; fi
        sleep 1
    done
fi
exec /data/apps/current/haunt
