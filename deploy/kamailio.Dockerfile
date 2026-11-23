# Kamailio signaling node with the nats module.
#
# Upstream CI alpine (ghcr.io/kamailio/kamailio-ci:6.1-alpine) still
# lacks nats.so. Debian trixie distro packages lag on 6.0.1. We pin the
# official Kamailio APT archive for the current stable series so the
# image tracks an exact release (nats + jansson modules included).
#
# Build:
#   docker build -f deploy/kamailio.Dockerfile \
#     -t gvos-kamailio:6.1.5-gvos.1 deploy/
# Tag policy: docs/edge-deps-fork-strategy.md

FROM debian:trixie-slim

ARG KAMAILIO_VER=6.1.5
# Debian package epoch suffix on the 6.1.5 trixie archive build.
ARG KAMAILIO_PKG_VER=6.1.5+bpo13

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl gnupg && \
    curl -fsSL https://deb.kamailio.org/kamailiodebkey.gpg \
      | gpg --dearmor -o /usr/share/keyrings/kamailio.gpg && \
    echo "deb [signed-by=/usr/share/keyrings/kamailio.gpg] https://deb-archive.kamailio.org/repos/kamailio-${KAMAILIO_VER} trixie main" \
      > /etc/apt/sources.list.d/kamailio.list && \
    printf 'Package: kamailio*\nPin: origin deb-archive.kamailio.org\nPin-Priority: 1001\n' \
      > /etc/apt/preferences.d/kamailio && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
        kamailio=${KAMAILIO_PKG_VER} \
        kamailio-nats-modules=${KAMAILIO_PKG_VER} \
        kamailio-extra-modules=${KAMAILIO_PKG_VER} \
        kamailio-utils-modules=${KAMAILIO_PKG_VER} \
        kamailio-websocket-modules=${KAMAILIO_PKG_VER} \
        kamailio-tls-modules=${KAMAILIO_PKG_VER} \
        kamailio-json-modules=${KAMAILIO_PKG_VER} \
        kamailio-postgres-modules=${KAMAILIO_PKG_VER} && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Config and certificates are volume-mounted at runtime (see compose);
# the packaged /etc/kamailio stays as an untouched fallback. The
# entrypoint materializes @NODE@ (per-node NATS subjects) into a
# runtime copy before exec.
COPY kamailio-entrypoint.sh /usr/local/bin/gvos-kamailio-entrypoint
RUN chmod +x /usr/local/bin/gvos-kamailio-entrypoint

EXPOSE 5060/udp 5060/tcp 4444/tcp

ENTRYPOINT ["gvos-kamailio-entrypoint"]
