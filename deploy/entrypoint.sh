#!/bin/sh
# rtpengine container entry: resolve our own IP (rtpengine needs a
# concrete interface address, not 0.0.0.0) and run in the foreground.
# Recording files land in /recording (compose volume).
#
# Tuning notes (bench + cloud ceiling 2026-11):
# - Port range 30000-60000 (30k): deleted-but-idle sessions hold sockets
#   until --timeout reaps them; steady-state ≈ CPS × timeout × ~4 ports.
# - --timeout 60 / --delete-delay 5: reclaim faster under BYE-loss tails
#   without cutting short normal hold music / transfer gaps.
# - --tos 184 (EF): prefer QoS marking on underlay when permitted.
# - --dtls-passive: accept DTLS-SRTP offers (SRTP/DTLS E2E path).
# - --log-level 5: cut NOTICE spam under 100CPS (was 6); raise via
#   GVOS_RTPENGINE_LOG_LEVEL for forensics.
# - Userspace only in-container (no xt_RTPENGINE); kernel offload is a
#   host-pool concern (see docs/edge-deps-fork-strategy.md).
set -e
mkdir -p /recording
IP=$(hostname -i | awk '{print $1}')
LOG_LEVEL="${GVOS_RTPENGINE_LOG_LEVEL:-5}"
PORT_MIN="${GVOS_RTPENGINE_PORT_MIN:-30000}"
PORT_MAX="${GVOS_RTPENGINE_PORT_MAX:-60000}"
TIMEOUT="${GVOS_RTPENGINE_TIMEOUT:-60}"
DELETE_DELAY="${GVOS_RTPENGINE_DELETE_DELAY:-5}"

# Optional extra flags (space-separated), e.g. "--homer 10.0.0.1:9060"
# shellcheck disable=SC2086
exec rtpengine --interface "$IP" --listen-ng 2223 \
    --listen-cli 127.0.0.1:2224 \
    --port-min "$PORT_MIN" --port-max "$PORT_MAX" \
    --log-level "$LOG_LEVEL" --foreground \
    --delete-delay "$DELETE_DELAY" --timeout "$TIMEOUT" \
    --tos 184 \
    --dtls-passive \
    --recording-dir /recording --recording-method pcap --recording-format eth \
    $GVOS_RTPENGINE_EXTRA_ARGS
