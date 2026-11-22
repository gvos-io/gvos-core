#!/usr/bin/env bash
# 10-round capacity benchmark with per-round telemetry:
# SIPp stats + CDR/ledger parity + Kamailio/NATS/PG/Redis counters.
# Usage: bash bench10.sh "40"   (rate in cps; 10 rounds)
set -uo pipefail

RATE="${1:-40}"
SCENDIR="$(cd "$(dirname "$0")" && pwd -W 2>/dev/null || pwd)"
DEST=kamailio
ROUNDS=10
N=$(( RATE * 10 ))   # 10 seconds of traffic per round

PSQL() { docker exec gvos-postgres-1 psql -U gvos -tA -c "$1"; }

echo "gvos 10-round benchmark - $(date -u +%FT%TZ) - rate=${RATE}cps x${ROUNDS} rounds x ${N} calls"
printf "%-6s %-7s %-8s %-8s %-9s %-10s %-8s %-8s %-10s\n" "round" "calls" "success" "failed" "setup(ms)" "cdr" "ledger" "kam_err" "nats_pending"

for i in $(seq 1 $ROUNDS); do
  CDR0=$(PSQL "SELECT count(*) FROM cdrs;")
  LED0=$(PSQL "SELECT count(*) FROM balance_ledger;")
  ERR0=$(docker logs gvos-kamailio-1 2>&1 | grep -cE "L_ERR|ERROR" || true)

  RAW=$(MSYS_NO_PATHCONV=1 docker run --rm --network gvos_default \
    -v "${SCENDIR}:/scen:ro" ctaloi/sipp:latest \
    -cid_str "%u-b10-${i}-$(date +%s)@%s" \
    -sf /scen/load.xml -inf /scen/dest.csv -m $N -r $RATE -timeout 30s $DEST 2>&1)
  sleep 3   # let CDR writers drain

  SUCC=$(echo "$RAW" | grep "Successful call" | tail -1 | awk '{print $NF}')
  FAIL=$(echo "$RAW" | grep "Failed call"     | tail -1 | awk '{print $NF}')
  TOT=$(echo "$RAW"  | grep "Total Call created" | tail -1 | awk '{print $NF}')
  RT=$(echo "$RAW"  | grep "Response Time"   | tail -1 | awk '{print $NF}')
  [ -z "$SUCC" ] && SUCC=0
  [ -z "$FAIL" ] && FAIL=0
  [ -z "$TOT" ] && TOT=0

  CDR1=$(PSQL "SELECT count(*) FROM cdrs;")
  LED1=$(PSQL "SELECT count(*) FROM balance_ledger;")
  ERR1=$(docker logs gvos-kamailio-1 2>&1 | grep -cE "L_ERR|ERROR" || true)

  printf "%-6s %-7s %-8s %-8s %-9s %-10s %-8s %-8s %-10s\n" \
    "$i" "$TOT" "$SUCC" "$FAIL" "${RT:-n/a}" \
    "$((CDR1-CDR0))" "$((LED1-LED0))" "$((ERR1-ERR0))" \
    "$(docker exec gvos-gv-core-1 sh -c 'wget -qO- http://nats-1:8222/jsz 2>/dev/null' | grep -o '\"pending\":*[0-9]*' | head -1 | grep -o '[0-9]*' || echo '?')"

  echo "$RAW" > "/tmp/bench10-round-${i}.log"
done
echo "done. per-round SIPp logs in /tmp/bench10-round-*.log"
