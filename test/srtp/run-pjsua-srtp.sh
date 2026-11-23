#!/usr/bin/env bash
# E2E: pjsua SRTP-mandatory → kamailio → demo-gw (RTP plaintext).
# Requires: gvos compose up, account 1000 seeded, image gvos-pjsua built.
set -euo pipefail
NET="${GVOS_DOCKER_NET:-gvos_default}"
DEST="${GVOS_SIP_HOST:-kamailio}"
echo "Building pjsua image if missing..."
docker image inspect gvos-pjsua >/dev/null 2>&1 || \
  docker build -f "$(dirname "$0")/pjsua.Dockerfile" -t gvos-pjsua "$(dirname "$0")"

# Minimal auto-answer + one outbound call; SRTP mandatory.
# Credentials: demo seed user 1000 / secret often in migrations seed.
cat > /tmp/gvos-pjsua.cfg <<'EOF'
--null-audio
--auto-answer=200
--use-srtp=2
--srtp-secure=1
--log-level=4
--app-log-level=3
--duration=8
--auto-play
EOF

docker run --rm --network "$NET" \
  -v /tmp/gvos-pjsua.cfg:/cfg:ro \
  gvos-pjsua \
  --config-file /cfg \
  "sip:8613800138000@${DEST}" \
  --id "sip:1000@${DEST}" \
  --realm "*" --username "1000" --password "1000" \
  2>&1 | tee /tmp/gvos-pjsua.log

if grep -qiE 'SRTP status:.*Active|DTLS-SRTP|secure call' /tmp/gvos-pjsua.log; then
  echo "PASS: SRTP active indicators found"
  exit 0
fi
echo "FAIL: no SRTP Active in pjsua log — inspect /tmp/gvos-pjsua.log"
exit 1
