#!/bin/bash
set -Eeuo pipefail
umask 077
fail() { echo "Haunt container: $*" >&2; exit 1; }
[[ $# == 0 ]] || { exec "$@"; }
[[ $(id -u) == 0 ]] || fail 'Startup needs root to prepare the volume; services run as separate unprivileged users.'

postgres_pid=''
# Both helpers are called by the EXIT trap (ShellCheck cannot trace it).
# shellcheck disable=SC2317
stop_process() {
    local pid=$1 signal=$2 seconds=$3
    [[ -n $pid ]] || return 0
    kill -"$signal" "$pid" 2>/dev/null || true
    for ((i=0; i<seconds*10; i++)); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.1
    done
    kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
}
# shellcheck disable=SC2317
cleanup() {
    trap - EXIT INT TERM
    rm -f /run/haunt/started
    stop_process "$postgres_pid" INT 15
}
trap cleanup EXIT
trap 'exit 0' TERM INT

install -d -m 755 /data /run/haunt /run/haunt/acme
rm -f /run/haunt/started /run/haunt/tls-ready /run/haunt/local-database
install -d -m 700 /data/config
install -d -m 700 -o haunt -g haunt /data/haunt
[[ ${HAUNT_DATA_DIR:-/data/haunt} == /data/haunt ]] || fail 'Use the /data volume; HAUNT_DATA_DIR must be /data/haunt in this image.'
[[ ${HAUNT_LISTEN_ADDR:-127.0.0.1:8080} == 127.0.0.1:8080 ]] || fail 'The internal API listener must stay at 127.0.0.1:8080.'
export HAUNT_DATA_DIR=/data/haunt HAUNT_LISTEN_ADDR=127.0.0.1:8080
export HAUNT_TRUSTED_PROXIES=127.0.0.1/32,::1/128

# Persist installation identity independently of container/image lifetimes.
if [[ -f /data/config/instance-id ]]; then
    saved_id=$(cat /data/config/instance-id)
    [[ -z ${HAUNT_INSTANCE_ID:-} || $HAUNT_INSTANCE_ID == "$saved_id" ]] || fail 'HAUNT_INSTANCE_ID differs from the saved installation.'
else
    saved_id=${HAUNT_INSTANCE_ID:-$(cat /proc/sys/kernel/random/uuid)}
    printf '%s\n' "$saved_id" > /data/config/instance-id.new
    mv /data/config/instance-id.new /data/config/instance-id
fi
export HAUNT_INSTANCE_ID=$saved_id

if [[ -n ${HAUNT_BOOTSTRAP_PASSWORD_FILE:-} ]]; then
    [[ -z ${HAUNT_BOOTSTRAP_PASSWORD:-} ]] || fail 'Use either bootstrap password or password file, not both.'
    HAUNT_BOOTSTRAP_PASSWORD=$(cat "$HAUNT_BOOTSTRAP_PASSWORD_FILE")
    export HAUNT_BOOTSTRAP_PASSWORD
fi
# Interactive setup creates the owner. Explicit secrets remain available for
# unattended installs; never generate or print a default owner password.
if [[ -n ${HAUNT_BOOTSTRAP_PASSWORD:-} ]]; then
    export HAUNT_BOOTSTRAP_USERNAME=${HAUNT_BOOTSTRAP_USERNAME:-owner}
fi

url=${HAUNT_PUBLIC_URL:-}
# Values interpolated into Nginx/ACME must not contain configuration syntax.
# Full origin/HTTPS validation is also performed by the application.
[[ $url =~ ^(https?)://(\[[0-9a-fA-F:]+\]|[a-zA-Z0-9.-]+)(:[0-9]{1,5})?$ ]] || fail 'Set HAUNT_PUBLIC_URL to an HTTP(S) origin without a trailing slash, path or credentials.'
scheme=${BASH_REMATCH[1]}
export HAUNT_TLS_HOST=${BASH_REMATCH[2]}
tls_mode=${HAUNT_TLS_MODE:-auto}
case $tls_mode in auto|provided|proxy) ;; *) fail 'HAUNT_TLS_MODE must be auto, provided, or proxy.' ;; esac
if [[ $scheme == http ]]; then
    [[ $tls_mode == auto || $tls_mode == proxy ]] || fail 'Provided TLS needs an HTTPS public URL.'
    tls_mode=proxy
fi
if [[ $tls_mode == auto ]]; then
    [[ $HAUNT_TLS_HOST =~ [a-zA-Z] && $HAUNT_TLS_HOST == *.* && $HAUNT_TLS_HOST != *:* ]] || fail 'Automatic HTTPS needs a public DNS hostname. Use provided certificates or an existing HTTPS proxy for local IPs.'
fi

cat > /run/haunt/proxy-trust.conf <<'NGINX'
geo $realip_remote_addr $haunt_trusted_proxy {
    default 0;
NGINX
if [[ -n ${HAUNT_PROXY_CIDRS:-} ]]; then
    IFS=, read -ra cidrs <<< "$HAUNT_PROXY_CIDRS"
    for cidr in "${cidrs[@]}"; do
        [[ $cidr =~ ^[0-9a-fA-F.:]+/[0-9]{1,3}$ && $cidr != */0 ]] || fail 'HAUNT_PROXY_CIDRS must contain explicit comma-separated CIDRs, never a catch-all.'
        printf '%s 1;\n' "$cidr" >> /run/haunt/proxy-trust.conf
    done
fi
cat >> /run/haunt/proxy-trust.conf <<'NGINX'
}
map "$haunt_trusted_proxy:$http_x_forwarded_proto" $haunt_forwarded_proto {
    default $scheme;
    "1:https" https;
}
NGINX
if [[ -n ${HAUNT_PROXY_CIDRS:-} ]]; then
    IFS=, read -ra cidrs <<< "$HAUNT_PROXY_CIDRS"
    for cidr in "${cidrs[@]}"; do
        [[ $cidr =~ ^[0-9a-fA-F.:]+/[0-9]{1,3}$ && $cidr != */0 ]] || fail 'HAUNT_PROXY_CIDRS must contain explicit comma-separated CIDRs, never a catch-all.'
        printf 'set_real_ip_from %s;\n' "$cidr" >> /run/haunt/proxy-trust.conf
    done
    if [[ ${HAUNT_CLOUDFLARED_ENABLED:-false} == true ]]; then
        echo 'real_ip_header CF-Connecting-IP;' >> /run/haunt/proxy-trust.conf
    else
        echo 'real_ip_header X-Forwarded-For;' >> /run/haunt/proxy-trust.conf
    fi
fi

if [[ -z ${HAUNT_DATABASE_URL:-} ]]; then
    export PGDATA=/data/postgres PGHOST=/run/postgresql PGPORT=5432
    /opt/haunt/database.sh init
    su-exec postgres postgres -D "$PGDATA" -c listen_addresses= -c unix_socket_directories="$PGHOST" &
    postgres_pid=$!
    for ((attempt=0; attempt<60; attempt++)); do
        kill -0 "$postgres_pid" 2>/dev/null || fail 'PostgreSQL exited during startup.'
        if su-exec postgres pg_isready -q -h "$PGHOST"; then break; fi
        sleep 1
    done
    su-exec postgres pg_isready -q -h "$PGHOST" || fail 'PostgreSQL did not become ready.'
    /opt/haunt/database.sh provision
    stop_process "$postgres_pid" INT 15
    postgres_pid=''
    export HAUNT_CONTAINER_LOCAL_DATABASE=1
    touch /run/haunt/local-database
    export HAUNT_DATABASE_URL='postgres://haunt@/haunt?host=/run/postgresql&sslmode=disable'
else
    export HAUNT_CONTAINER_LOCAL_DATABASE=0
fi

export HAUNT_TLS_MODE=$tls_mode
haunt-update seed
/opt/haunt/listeners.sh
nginx -c /opt/haunt/nginx.conf -t
cp /opt/haunt/supervisord.conf /run/haunt/supervisord.conf
if [[ $HAUNT_CONTAINER_LOCAL_DATABASE == 1 ]]; then
    cat /opt/haunt/postgres.conf >> /run/haunt/supervisord.conf
fi
if [[ $tls_mode == auto ]]; then
    cat /opt/haunt/acme.conf >> /run/haunt/supervisord.conf
else
    touch /run/haunt/tls-ready
fi
if [[ -n ${HAUNT_APP_UPDATE_MANIFEST_URL:-}${HAUNT_APP_UPDATE_REPOSITORY:-} ]]; then
    cat /opt/haunt/updater.conf >> /run/haunt/supervisord.conf
fi
trap - EXIT INT TERM
exec supervisord -n -c /run/haunt/supervisord.conf
