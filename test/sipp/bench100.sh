#!/usr/bin/env bash
# 100CPS × 10,000-call capacity benchmark (post-fix validation).
# Each round: SIPp stats + CDR/ledger parity + quality gates
# (runaway=0, CDRR redeliveries, duration=0 share, answer_time coverage).
# Usage: bash bench100.sh [rounds]   (default 1 round of 10k calls @ 100cps)
set -uo pipefail

ROUNDS="${1:-1}"
RATE=100
CALLS=10000
SCENDIR="$(cd "$(dirname "$0")" && pwd -W 2>/dev/null || pwd)"
DEST=kamailio
# -l 800: SIPp's default (3×rate=300) throttles at 100CPS with ~6s
# call length; 800 keeps the limiter out of the measurement.
# -cid_str MUST contain %u (unique per call): a constant prefix makes
# every call share one Call-ID — BYE retransmission fog, 100% failure.
CIDFMT="%u-b100-r${ROUNDS}-$(date +%s)@%s"

PSQL() { docker exec gvos-postgres-1 psql -U gvos -tA -c "$1"; }

log() { docker logs gvos-kamailio-1 --since 30m 2>&1; }

echo "G-VOS 100CPS benchmark - $(date -u +%FT%TZ) - rate=${RATE}cps x${ROUNDS} rounds x ${CALLS} calls"
printf "%-6s %-7s %-8s %-8s %-9s %-7s %-7s %-8s %-10s %-11s %-10s\n" \
  "round" "calls" "success" "failed" "setup(ms)" "cdr" "ledger" "runaway" "redeliv" "dur0_pct" "ans_cov"

for i in $(seq 1 $ROUNDS); do
  CDR0=$(PSQL "SELECT count(*) FROM cdr;")
  LED0=$(PSQL "SELECT count(*) FROM balance_ledger;")
  CREDIT0=$(PSQL "SELECT credit FROM accounts WHERE username='1000';")
  RUN0=$(docker logs gvos-kamailio-1 2>&1 | grep -c "runaway")
  RDL0=$(docker exec gvos-gv-core-1 sh -c 'wget -qO- "http://nats-1:8222/jsz?consumers=true&streams=true"' 2>/dev/null | grep -o '"name": "CDRR"[^}]*' | grep -o '"num_redelivered": [0-9]*' | grep -o '[0-9]*' || echo 0)

  # ~100s of traffic at 100cps; give the tail 120s before judging.
  RAW=$(MSYS_NO_PATHCONV=1 docker run --rm --network gvos_default \
    -v "${SCENDIR}:/scen:ro" ctaloi/sipp:latest \
    -cid_str "%u-b100-${i}-$(date +%s)@%s" \
    -sf /scen/load.xml -inf /scen/dest.csv -m $CALLS -r $RATE -l 800 -timeout 180s $DEST 2>&1)

  SUCC=$(echo "$RAW" | grep "Successful call" | tail -1 | awk '{print $NF}')
  FAIL=$(echo "$RAW" | grep "Failed call"     | tail -1 | awk '{print $NF}')
  TOT=$(echo "$RAW"  | grep "Total Call created" | tail -1 | awk '{print $NF}')
  RT=$(echo "$RAW"  | grep "Response Time"   | tail -1 | awk '{print $NF}')
  [ -z "$SUCC" ] && SUCC=0
  [ -z "$FAIL" ] && FAIL=0
  [ -z "$TOT" ] && TOT=0
  echo "$RAW" > "/tmp/bench100-round-${i}.log"

  # Drain: CDRW/CDRR consumers settle a few seconds behind the last BYE.
  for w in 1 2 3 4 5 6; do
    sleep 10
    PEND=$(docker exec gvos-gv-core-1 sh -c 'wget -qO- "http://nats-1:8222/jsz?consumers=true&streams=true&config=true"' 2>/dev/null | python -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: print(-1); sys.exit()
pend=0
for acc in d.get('account_details',[]):
    for st in acc.get('stream_detail',[]):
        for c in st.get('consumer_detail',[]):
            pend += c.get('num_ack_pending',0) or 0
print(pend)" 2>/dev/null || echo -1)
    [ "${PEND:-99}" = "0" ] && break
  done

  CDR1=$(PSQL "SELECT count(*) FROM cdr;")
  LED1=$(PSQL "SELECT count(*) FROM balance_ledger;")
  RUN1=$(docker logs gvos-kamailio-1 2>&1 | grep -c "runaway")
  RDL1=$(docker exec gvos-gv-core-1 sh -c 'wget -qO- "http://nats-1:8222/jsz?consumers=true&streams=true"' 2>/dev/null | grep -o '"name": "CDRR"[^}]*' | grep -o '"num_redelivered": [0-9]*' | grep -o '[0-9]*' || echo 0)
  NEWCDR=$((CDR1-CDR0))
  DUR0=$(PSQL "SELECT count(*) FILTER (WHERE duration = 0) FROM cdr WHERE id > (SELECT max(id)-${NEWCDR} FROM cdr);" 2>/dev/null || echo -1)
  ANSC=$(PSQL "SELECT count(*) FILTER (WHERE answer_time IS NOT NULL) FROM cdr WHERE id > (SELECT max(id)-${NEWCDR} FROM cdr);" 2>/dev/null || echo -1)
  DUR0PCT=$([ "${NEWCDR:-0}" -gt 0 ] && echo $(( DUR0 * 100 / NEWCDR )) || echo 0)
  ANSPCT=$([ "${NEWCDR:-0}" -gt 0 ] && echo $(( ANSC * 100 / NEWCDR )) || echo 0)

  printf "%-6s %-7s %-8s %-8s %-9s %-7s %-7s %-8s %-10s %-11s %-10s\n" \
    "$i" "$TOT" "$SUCC" "$FAIL" "${RT:-n/a}" \
    "$NEWCDR" "$((LED1-LED0))" "$((RUN1-RUN0))" "$((RDL1-RDL0))" "${DUR0PCT}%(${DUR0}/${NEWCDR})" "${ANSPCT}%"

  docker exec gvos-postgres-1 psql -U gvos -c "
    SELECT min(pdd_ms), avg(pdd_ms)::int, percentile_disc(0.99) WITHIN GROUP (ORDER BY pdd_ms) AS pdd_p99,
           percentile_disc(0.99) WITHIN GROUP (ORDER BY duration) AS dur_p99
    FROM cdr WHERE id > (SELECT max(id)-${NEWCDR} FROM cdr);" 2>/dev/null
  docker exec gvos-postgres-1 psql -U gvos -tA -c "
    SELECT 'cdr='||count(*)||' billed_sum='||sum(billed_secs)||' cost_sum='||sum(cost)::numeric(12,0)
    FROM cdr WHERE id > (SELECT max(id)-${NEWCDR} FROM cdr); SELECT 'ledger='||count(*)||' delta_sum='||sum(delta)::numeric(12,0)
    FROM balance_ledger WHERE id > (SELECT max(id)-${NEWCDR} FROM balance_ledger);" 2>/dev/null
done
echo "done. per-round SIPp logs in /tmp/bench100-round-*.log"
