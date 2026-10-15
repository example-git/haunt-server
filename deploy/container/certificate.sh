#!/bin/bash
set -Eeuo pipefail
# The challenge webroot is public; account/keys remain in root-only directories.
umask 022
mode=${1:?issue or renew}
if [[ $mode == manage ]]; then
    for _ in $(seq 1 30); do
        [[ ! -f /run/haunt/nginx.pid ]] || break
        sleep 1
    done
    if [[ ! -s /data/tls/fullchain.pem || ! -f /data/config/tls-host || $(cat /data/config/tls-host) != "$HAUNT_TLS_HOST" ]]; then
        echo "Obtaining HTTPS certificate for $HAUNT_TLS_HOST; public TCP 80 must reach this container."
        timeout 180 /opt/haunt/certificate.sh issue
        printf '%s\n' "$HAUNT_TLS_HOST" > /data/config/tls-host
        /opt/haunt/listeners.sh
        nginx -c /opt/haunt/nginx.conf -t
        nginx -c /opt/haunt/nginx.conf -s reload
    fi
    touch /run/haunt/tls-ready
    exec /opt/haunt/certificate.sh renew
fi
args=(--home /data/acme --config-home /data/acme --cert-home /data/acme/certs --server letsencrypt)
mkdir -p /data/acme /data/tls
chmod 700 /data/acme /data/tls
if [[ $mode == issue ]]; then
    [[ -z ${HAUNT_ACME_EMAIL:-} ]] || args+=(--accountemail "$HAUNT_ACME_EMAIL")
    acme.sh "${args[@]}" --issue --webroot /run/haunt/acme -d "$HAUNT_TLS_HOST" --keylength ec-256
    # Renewal uses these installed paths and reload hook; never rely on acme.sh's
    # private certificate layout. Existing valid certs stay available on failure.
    acme.sh "${args[@]}" --install-cert -d "$HAUNT_TLS_HOST" --ecc \
        --key-file /data/tls/privkey.pem --fullchain-file /data/tls/fullchain.pem \
        --reloadcmd 'nginx -c /opt/haunt/nginx.conf -t && nginx -c /opt/haunt/nginx.conf -s reload'
elif [[ $mode == renew ]]; then
    while true; do
        if ! acme.sh "${args[@]}" --cron; then
            echo 'Certificate renewal failed; keeping the existing certificate and retrying in 12 hours.' >&2
        fi
        sleep 43200 & wait $!
    done
else
    exit 2
fi
