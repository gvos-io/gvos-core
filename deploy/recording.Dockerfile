# rtpengine-recording daemon built from the same upstream stable tag
# as gvos-rtpengine (mr26.3.1.4). Spool/output must be different dirs
# (inotify watches the spool; finished files land in output).
#
# Build:
#   docker build -f deploy/recording.Dockerfile \
#     -t gvos-recording:26.3.1.4-gvos.1 deploy/
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

WORKDIR /usr/src/rtpengine/recording-daemon
RUN make -j"$(nproc)" rtpengine-recording && \
    strip -o /usr/local/bin/rtpengine-recording rtpengine-recording

FROM debian:trixie-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ffmpeg \
        libavcodec-extra libavfilter10 libbcg729-0 \
        libcurl4t64 libevent-2.1-7t64 libevent-pthreads-2.1-7t64 \
        libglib2.0-0t64 libhiredis1.1.0 libip4tc2 libip6tc2 \
        libjson-glib-1.0-0 libjwt2 libmariadb3 libncursesw6 \
        libopus0 libpcap0.8t64 libpcre2-8-0 libspandsp2 \
        libssl3t64 libwebsockets19t64 \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=build /usr/local/bin/rtpengine-recording /usr/local/bin/rtpengine-recording

# Per-track files (every RTP SSRC its own file, --output-mixed off):
# the agent/user split is the uploader's job (DUX metadata + A-leg
# from-tag); MP3 64kbps mono is ~0.5MB/min and plays everywhere.
ENTRYPOINT ["rtpengine-recording", \
    "--spool-dir=/rec/spool", \
    "--output-dir=/rec/output", \
    "--output-storage=file", \
    "--output-format=mp3", \
    "--mp3-bitrate=64000", \
    "--output-single", \
    "--output-mixed=off", \
    "--output-pattern=%c-%t"]
