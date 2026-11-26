# pjsua with OpenSSL for SRTP/DTLS-SRTP E2E against gvos.
FROM debian:bookworm-slim
RUN apt-get update -qq && apt-get install -y --no-install-recommends \
    build-essential curl ca-certificates python3 \
    libssl-dev libasound2-dev libsdl2-dev libopus-dev && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /src
ARG PJ_VER=2.14
RUN curl -fsSL "https://github.com/pjsip/pjproject/archive/refs/tags/${PJ_VER}.tar.gz" | tar xz && \
    cd pjproject-${PJ_VER} && \
    # libopus-dev enables PJMEDIA_HAS_OPUS_CODEC (needed for Opus↔PCMU
    # transcode e2e; the previous image only had iLBC/G722 as non-PCMU).
    ./configure --disable-video --enable-shared --with-ssl && \
    make dep && make -j"$(nproc)" && make install && ldconfig && \
    # pjsua itself is a pjsip-apps sample: make install only ships the
    # libraries — link the binary into PATH by hand.
    cp pjsip-apps/bin/pjsua-x86_64-unknown-linux-gnu /usr/local/bin/pjsua
ENV LD_LIBRARY_PATH=/usr/local/lib
ENTRYPOINT ["pjsua"]
