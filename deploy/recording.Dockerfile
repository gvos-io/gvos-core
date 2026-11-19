# rtpengine-recording daemon (R0 POC, recording-oss-upgrade-plan §4):
# consumes the DUX spool (inotify) and transcodes to MP3/WAV.
# Debian bookworm ships rtpengine-recording-daemon 10.5.3.5 with
# ffmpeg/libmp3lame in the dependency chain (the 2026-11-19 binary
# audit: --output-storage is file|db|both only — S3 direct upload does
# not exist in distro packages, the Go uploader is the only OSS exit).
#
# spool and output MUST be different directories (inotify watches the
# spool; finished files land in output).
#
# Build: docker build -f deploy/recording.Dockerfile -t gvos-recording deploy/
# Run: two volumes — spool = the rtpengine --recording-dir, output = empty.

FROM debian:bookworm-slim

RUN apt-get update -qq && \
    apt-get install -y --no-install-recommends \
        rtpengine-recording-daemon ffmpeg && \
    rm -rf /var/lib/apt/lists/*

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
