# syntax=docker/dockerfile:1.7
# Build with the pinned client submodule initialized. No private plugins or
# installation data are copied into the build context (see .dockerignore).
FROM --platform=$BUILDPLATFORM node:22-alpine3.23 AS web
WORKDIR /src/client
ENV ELECTRON_SKIP_BINARY_DOWNLOAD=1
COPY client/package.json client/package-lock.json ./
COPY client/packages/core/package.json packages/core/package.json
COPY client/scripts/api-generator/package.json client/scripts/api-generator/package-lock.json scripts/api-generator/
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY client/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26.9-alpine3.23 AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY webclient_disabled.go ./
ARG TARGETOS
ARG TARGETARCH
ARG HAUNT_VERSION=container
ARG HAUNT_WEB_CLIENT_ORIGIN
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOMAXPROCS=2 \
    go build -p 2 -trimpath -ldflags="-s -w -X main.version=$HAUNT_VERSION -X github.com/example-git/haunt-server/internal/config.HostedClientOrigin=$HAUNT_WEB_CLIENT_ORIGIN" -o /haunt ./cmd/haunt

# Keep probing support without ffmpeg's device/filter stack or a video encoder
# executable. Haunt forwards media; it does not transcode calls on the server.
FROM alpine:3.23 AS probe
RUN apk add --no-cache build-base nasm curl xz zlib-dev ccache
WORKDIR /build
# Source hash matches Alpine's reviewed ffmpeg 8.0.1 source archive. Build only
# the file-probing program and builtin codecs, with no network/device/filter or
# external encoder dependencies. Preserve format detection without a GPU stack.
RUN --mount=type=cache,target=/root/.cache/ccache \
    curl -fsSLo ffmpeg.tar.xz https://ffmpeg.org/releases/ffmpeg-8.0.1.tar.xz \
    && echo '7af5cd1c7ba04f8d7ea1b54b64b9c235f2a606dc0c18d25cfdf20958bcad4851d3265b4894fc6ba5dd205c2309971c7e7fb890ee1d64e0a41a3faf159f77957f  ffmpeg.tar.xz' | sha512sum -c - \
    && tar -xf ffmpeg.tar.xz --strip-components=1 \
    && ./configure --cc='ccache cc' --disable-autodetect --enable-zlib --disable-programs --enable-ffprobe \
        --disable-network --disable-avdevice --disable-avfilter --disable-swscale \
        --disable-swresample --disable-encoders --disable-muxers --disable-doc --disable-debug \
    && make -j2 ffprobe \
    && strip ffprobe

FROM alpine:3.23
RUN apk add --no-cache bash ca-certificates curl tini su-exec nginx \
        postgresql16 postgresql16-client acme.sh supervisor python3 \
    && addgroup -S -g 10001 haunt \
    && adduser -S -D -H -u 10001 -G haunt haunt \
    && mkdir -p /usr/share/haunt/web /opt/haunt /data
COPY --from=probe /build/ffprobe /usr/bin/ffprobe
COPY --from=probe /build/COPYING.LGPLv2.1 /usr/share/licenses/ffprobe/COPYING.LGPLv2.1
COPY --from=server /haunt /usr/local/bin/haunt
COPY --from=web /src/client/dist/ /usr/share/haunt/web/
COPY deploy/container/ /opt/haunt/
ARG HAUNT_VERSION=0.0.0
RUN chmod 755 /opt/haunt/*.sh /opt/haunt/update.py \
    && ln -s /opt/haunt/update.py /usr/local/bin/haunt-update \
    && ln -sf /run/haunt/supervisord.conf /etc/supervisord.conf \
    && printf '%s\n' "$HAUNT_VERSION" > /opt/haunt/bundled-version \
    && ffprobe -version > /dev/null
ENV HAUNT_DATA_DIR=/data/haunt \
    HAUNT_LISTEN_ADDR=127.0.0.1:8080 \
    HAUNT_TRUSTED_PROXIES=127.0.0.1/32,::1/128 \
    HAUNT_MEDIA_PORT_MIN=50000 \
    HAUNT_MEDIA_PORT_MAX=50100
EXPOSE 80/tcp 443/tcp 50000-50100/udp
VOLUME ["/data"]
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 \
    CMD ["/opt/haunt/healthcheck.sh"]
ENTRYPOINT ["/sbin/tini", "--", "/opt/haunt/entrypoint.sh"]
