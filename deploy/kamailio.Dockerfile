# Kamailio signaling node with the nats module.
#
# The upstream CI alpine image (ghcr.io/kamailio/kamailio-ci:6.1-alpine)
# does NOT ship nats.so (checked 2026-11-19: 237 modules, kafka/mqtt/
# rabbitmq present, nats absent; Alpine 3.24/edge repos carry no
# kamailio-nats package either). Debian does, so the signaling plane
# builds from here:
#   - bookworm: nats yes, jansson NO (no package at all)
#   - trixie:   nats yes (kamailio-nats-modules) + jansson in
#               kamailio-extra-modules, Kamailio 6.0.1
# Hence trixie.
#
# Build: docker build -f deploy/kamailio.Dockerfile -t gvos-kamailio deploy/
# (context must cover deploy/ so the cfg and tls material mount cleanly;
# runtime mounts overwrite /etc/kamailio anyway).

FROM debian:trixie-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        kamailio \
        kamailio-nats-modules \
        kamailio-extra-modules \
        kamailio-utils-modules \
        kamailio-websocket-modules \
        kamailio-tls-modules \
        kamailio-json-modules \
        kamailio-postgres-modules && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Config and certificates are volume-mounted at runtime (see compose);
# the packaged /etc/kamailio stays as an untouched fallback.

EXPOSE 5060/udp 5060/tcp 4444/tcp

ENTRYPOINT ["kamailio", "-DD", "-E", "-f", "/etc/kamailio/kamailio.cfg"]
