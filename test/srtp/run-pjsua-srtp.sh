#!/usr/bin/env bash
# E2E: pjsua SRTP-mandatory → kamailio/rtpengine → demo-gw (RTP plaintext).
# Requires: capbench (or gvos) compose up, account 1000/demo1000, gvos-pjsua image.
#
# Pitfalls (2026-11-26):
#   --srtp-secure=0   (=1 demands TLS signalling → ESESSIONINSECURE)
#   --realm gvos.local; password demo1000 (not 1000)
#   --registrar required or 401 is not retried
#   dial 8613800130000 (demo-gw); 8613800138000 is DID loopback → 480
#   register BEFORE dial (stdin sleep + m) — CLI URI dials pre-reg → 487
set -euo pipefail
NET="${GVOS_DOCKER_NET:-capbench_default}"
DEST="${GVOS_SIP_HOST:-kamailio}"
CMDS="$(dirname "$0")/pjsua-srtp-cmds.txt"

echo "Building pjsua image if missing..."
docker image inspect gvos-pjsua >/dev/null 2>&1 || \
  docker build -f "$(dirname "$0")/pjsua.Dockerfile" -t gvos-pjsua "$(dirname "$0")"

# Rewrite dial target into a temp cmd file when DEST differs.
TMP_CMDS="$(mktemp)"
sed "s/@kamailio/@${DEST}/g; s#sip:8613800130000@kamailio#sip:8613800130000@${DEST}#" \
  "$CMDS" > "$TMP_CMDS"

LOG="$(mktemp)"
set +e
docker run -i --rm --network "$NET" gvos-pjsua \
  --null-audio \
  --use-srtp=2 \
  --srtp-secure=0 \
  --log-level=4 \
  --app-log-level=3 \
  --id "sip:1000@${DEST}" \
  --registrar "sip:${DEST}" \
  --realm "gvos.local" \
  --username "1000" \
  --password "demo1000" \
  < "$TMP_CMDS" 2>&1 | tee "$LOG"
rc=${PIPESTATUS[0]}
set -e
rm -f "$TMP_CMDS"

if grep -qiE 'SRTP status:.*Active' "$LOG"; then
  echo "PASS: SRTP Active"
  rm -f "$LOG"
  exit 0
fi
echo "FAIL: no SRTP Active in pjsua log — inspect $LOG"
exit 1
