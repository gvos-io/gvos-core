#!/usr/bin/env bash
# E2E: pjsua (non-PCMU) → kamailio/rtpengine (codec-mask-all +
# codec-transcode-PCMU) → SIPp UAS (PCMU). Asserts the answer restores
# the offerer's codec (transcoding engaged) and SRTP stays Active.
#
# Default codec: G722 (always present in stock pjsua). Prefer Opus when
# the image was built with libopus-dev (see pjsua.Dockerfile).
set -euo pipefail
NET="${GVOS_DOCKER_NET:-capbench_default}"
DEST="${GVOS_SIP_HOST:-kamailio}"
CODEC="${GVOS_TRANSCODE_CODEC:-g722}"   # g722 | opus

case "$CODEC" in
  opus)
    DIS=(--dis-codec pcmu --dis-codec pcma --dis-codec gsm --dis-codec g722 \
         --dis-codec speex --dis-codec ilbc --dis-codec l16)
    WANT_RTPMAP='opus/'
    ;;
  g722|*)
    DIS=(--dis-codec pcmu --dis-codec pcma --dis-codec gsm --dis-codec speex \
         --dis-codec ilbc --dis-codec l16)
    WANT_RTPMAP='G722/'
    ;;
esac

CMDS="$(mktemp)"
cat >"$CMDS" <<EOF
sleep 2500
m
sip:8613800130000@${DEST}
sleep 7000
dq
ha
q
EOF

LOG="$(mktemp)"
set +e
docker run -i --rm --network "$NET" gvos-pjsua \
  --null-audio --use-srtp=2 --srtp-secure=0 \
  "${DIS[@]}" \
  --log-level=5 --app-log-level=4 \
  --id "sip:1000@${DEST}" --registrar "sip:${DEST}" \
  --realm gvos.local --username 1000 --password demo1000 \
  <"$CMDS" 2>&1 | tee "$LOG"
set -e
rm -f "$CMDS"

ok=1
grep -q "SDP negotiation done: Success" "$LOG" || ok=0
grep -qiE "SRTP status:.*Active" "$LOG" || ok=0
grep -q "a=rtpmap:.*${WANT_RTPMAP}" "$LOG" || ok=0
# Answer must NOT be bare PCMU toward the offerer when we forced a non-PCMU offer.
if grep -E "RX .*200/INVITE" -A20 "$LOG" | grep -q 'a=rtpmap:0 PCMU'; then
  # only fail if that PCMU line is the sole audio rtpmap in the 200
  if ! grep -E "RX .*200/INVITE" -A20 "$LOG" | grep -q "a=rtpmap:.*${WANT_RTPMAP}"; then
    ok=0
  fi
fi

if [[ "$ok" -eq 1 ]]; then
  echo "PASS: transcode ${CODEC}↔PCMU + SRTP Active"
  rm -f "$LOG"
  exit 0
fi
echo "FAIL: see $LOG"
exit 1
