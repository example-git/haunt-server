#!/bin/sh
set -eu
test -s /data/apps/current/web/index.html
test -f /run/haunt/started
exec curl --fail --silent --show-error --max-time 4 http://127.0.0.1:8081/readyz
