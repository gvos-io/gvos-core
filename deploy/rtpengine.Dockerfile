# RTPengine userspace daemon built from the upstream stable tag.
# Debian bookworm packages lag on 10.5.x; we compile the current
# Sipwise stable release so ng-protocol / DTLS-SRTP stay aligned with
# Kamailio 6.1.x.
#
# Build:
#   docker build -f deploy/rtpengine.Dockerfile \
#     -t gvos-rtpengine:26.3.1.4-gvos.1 deploy/
# Tag policy: docs/edge-deps-fork-strategy.md
#
# Override source (e.g. org fork or mirror):
#   --build-arg RTPENGINE_GIT_URL=https://github.com/gvos-io/rtpengine.git
#   --build-arg RTPENGINE_REF=mr26.3.1.4

ARG RTPENGINE_REF=mr26.3.1.4
ARG RTPENGINE_GIT_URL=https://github.com/sipwise/rtpengine.git

FROM debian:trixie-slim AS build

ARG RTPENGINE_REF
ARG RTPENGINE_GIT_URL
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        build-essential ca-certificates curl g++ gcc git gperf make \
        default-libmysqlclient-dev \
        libavcodec-extra libavfilter-dev libbcg729-dev \
        libcurl4-openssl-dev libevent-dev libglib2.0-dev \
        libhiredis-dev libiptc-dev libjson-glib-dev libjwt-dev \
        libncursesw5-dev libopus-dev libpcap-dev libpcre2-dev \
        libspandsp-dev libssl-dev libwebsockets-dev \
        markdown patch pkg-config && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /usr/src
RUN git clone --depth 1 --branch "${RTPENGINE_REF}" \
        "${RTPENGINE_GIT_URL}" rtpengine

WORKDIR /usr/src/rtpengine/daemon
RUN make -j"$(nproc)" rtpengine && \
    strip -o /usr/local/bin/rtpengine rtpengine

FROM debian:trixie-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        libavcodec-extra libavfilter10 libbcg729-0 \
        libcurl4t64 libevent-2.1-7t64 libevent-pthreads-2.1-7t64 \
        libglib2.0-0t64 libhiredis1.1.0 libip4tc2 libip6tc2 \
        libjson-glib-1.0-0 libjwt2 libmariadb3 libncursesw6 \
        libopus0 libpcap0.8t64 libpcre2-8-0 libspandsp2 \
        libssl3t64 libwebsockets19t64 \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=build /usr/local/bin/rtpengine /usr/local/bin/rtpengine
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# userspace only (no xt_RTPENGINE in-container); interface IP is
# discovered at entry (rtpengine refuses 0.0.0.0 as interface).
ENTRYPOINT ["/entrypoint.sh"]
