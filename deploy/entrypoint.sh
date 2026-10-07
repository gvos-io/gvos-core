#!/bin/sh
# rtpengine container entry: resolve our own IP (rtpengine needs a
# concrete interface address, not 0.0.0.0) and run in the foreground.
# Recording files land in /recording (compose volume).
set -e
mkdir -p /recording
IP=$(hostname -i | awk '{print $1}')
exec rtpengine --interface "$IP" --listen-ng 2223 \
    --log-level 6 --foreground --delete-delay 0 --timeout 60 \
    --recording-dir /recording --recording-method pcap --recording-format eth
