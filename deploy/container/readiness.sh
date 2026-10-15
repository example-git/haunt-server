#!/bin/sh
set -eu
for _ in $(seq 1 210); do
    if [ -f /run/haunt/tls-ready ] && curl --fail --silent --max-time 2 http://127.0.0.1:8081/readyz >/dev/null; then
        touch /data/config/bootstrapped /run/haunt/started
        echo "Haunt is ready at $HAUNT_PUBLIC_URL"
        exit 0
    fi
    sleep 1
done
echo 'Haunt readiness timed out; inspect supervisorctl status and the service logs.' >&2
exit 1
