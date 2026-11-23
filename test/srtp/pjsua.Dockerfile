# pjsua with OpenSSL for SRTP/DTLS-SRTP E2E against gvos.
FROM debian:bookworm-slim
RUN apt-get update -qq && apt-get install -y --no-install-recommends \
    build-essential curl ca-certificates python3 \
    libssl-dev libasound2-dev libsdl2-dev && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /src
ARG PJ_VER=2.14
RUN curl -fsSL "https://github.com/pjsip/pjproject/archive/refs/tags/${PJ_VER}.tar.gz" | tar xz && \
    cd pjproject-${PJ_VER} && \
    ./configure --disable-video --enable-shared --with-ssl && \
    make dep && make -j"$(nproc)" && make install && ldconfig
ENV LD_LIBRARY_PATH=/usr/local/lib
ENTRYPOINT ["pjsua"]
