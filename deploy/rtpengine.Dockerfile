# Minimal rtpengine daemon image from Debian packages.
# Build: docker build -f deploy/rtpengine.Dockerfile -t gvos-rtpengine deploy/

FROM debian:bookworm-slim

RUN apt-get update -qq && \
    apt-get install -y --no-install-recommends rtpengine-daemon && \
    rm -rf /var/lib/apt/lists/*

# userspace only (no kernel-module forwarding in the container);
# the container's own network IP is discovered at entry (rtpengine
# refuses 0.0.0.0 as interface).
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
