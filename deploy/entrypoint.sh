#!/bin/sh
# rtpengine container entry: resolve our own IP (rtpengine needs a
# concrete interface address, not 0.0.0.0) and run in the foreground.
# Recording files land in /recording (compose volume).
# Port range 30000-60000 (30k): a deleted-but-idle media session
# still holds its sockets until the media-inactivity timeout
# (--timeout) reaps it, so steady-state port demand is roughly
# CPS x timeout x ~4 ports/call. The daemon's default 10k range
# covers ~40CPS at 60s; 100CPS needs ~24k — 30k leaves margin.
set -e
mkdir -p /recording
IP=$(hostname -i | awk '{print $1}')
exec rtpengine --interface "$IP" --listen-ng 2223 \
    --port-min 30000 --port-max 60000 \
    --log-level 6 --foreground --delete-delay 10 --timeout 60 \
    --recording-dir /recording --recording-method pcap --recording-format eth
