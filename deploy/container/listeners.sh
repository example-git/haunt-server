#!/bin/bash
set -Eeuo pipefail
fail() { echo "$*" >&2; exit 1; }
tls_mode=${HAUNT_TLS_MODE:?}
url=${HAUNT_PUBLIC_URL:?}
    if [[ $tls_mode == proxy ]]; then
        cat > /run/haunt/listeners.conf <<'NGINX'
server { listen 80; listen [::]:80; server_name _; include /opt/haunt/routes.conf; }
NGINX
    elif [[ ! -s /data/tls/fullchain.pem || ! -s /data/tls/privkey.pem ]]; then
        [[ $tls_mode != provided ]] || fail 'Mount fullchain.pem and privkey.pem under /data/tls for provided TLS.'
        cat > /run/haunt/listeners.conf <<'NGINX'
server {
    listen 80; listen [::]:80; server_name _;
    location ^~ /.well-known/acme-challenge/ { root /run/haunt/acme; }
    location / { return 503; }
}
NGINX
    else
        cat > /run/haunt/listeners.conf <<NGINX
server {
    listen 80; listen [::]:80; server_name _;
    location ^~ /.well-known/acme-challenge/ { root /run/haunt/acme; }
    location / { return 308 $url\$request_uri; }
}
server {
    listen 443 ssl; listen [::]:443 ssl; server_name _;
    ssl_certificate /data/tls/fullchain.pem;
    ssl_certificate_key /data/tls/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:haunt_tls:1m;
    include /opt/haunt/routes.conf;
}
NGINX
    fi
