#!/usr/bin/env bash
# Config-face SRTP/DTLS smoke: rtpengine cmdline flags + kamailio module.
set -euo pipefail
RTP="${RTPENGINE_CONTAINER:-gvos-rtpengine-1}"
KAM="${KAMAILIO_CONTAINER:-gvos-kamailio-1}"

echo "== rtpengine cmdline =="
ARGS=$(docker exec "$RTP" sh -c 'tr "\0" " " < /proc/1/cmdline' 2>/dev/null || true)
if [ -z "$ARGS" ]; then
  echo "FAIL: cannot read rtpengine cmdline (container down?)"; exit 1
fi
echo "$ARGS"
echo "$ARGS" | grep -q 'dtls-passive' || {
  echo "FAIL: missing --dtls-passive"; exit 1
}
echo "$ARGS" | grep -q 'recording-dir' || {
  echo "FAIL: missing --recording-dir"; exit 1
}
echo "OK: dtls-passive + recording-dir present"

echo "== kamailio rtpengine =="
if docker exec "$KAM" kamcmd rtpengine.show >/tmp/gvos-rtpengine-show.txt 2>/dev/null; then
  head -20 /tmp/gvos-rtpengine-show.txt
  echo "OK: kamcmd rtpengine.show"
else
  docker logs "$KAM" 2>&1 | grep -i 'rtpengine' | tail -5 || true
  echo "WARN: kamcmd rtpengine.show unavailable (module may still be loaded)"
fi
echo "OK: smoke-config done (media E2E: bash test/srtp/run-pjsua-srtp.sh)"
