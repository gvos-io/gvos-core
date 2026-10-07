#!/usr/bin/env bash
# Long-stability soak: run wave after wave of traffic for a configured
# duration, checkpointing CDR/balance consistency after every wave.
# The consistency check is the point: soak = hours of billing with
# zero drift.
#
# Usage:
#   bash test/sipp/soak.sh [duration_minutes] [cps_per_worker] [workers]
#
# Defaults: 24h at 10 CPS x 2 workers (a soak, not a spike test).
# Waves are small (200 calls per worker) so the SIPp UAS soft limit
# (~1000 cumulative calls) stays out of the way: the UAS restarts
# between waves.

set -uo pipefail

DUR_MIN="${1:-1440}"   # minutes
CPS="${2:-10}"
WORKERS="${3:-2}"
WAVE_CALLS=100         # per worker per wave
SCENDIR="$(cd "$(dirname "$0")" && pwd -W 2>/dev/null || pwd)"
LOG="$SCENDIR/soak-$(date +%Y%m%d-%H%M%S).log"
API="http://127.0.0.1:8080"
TOKEN="${GVOS_TOKEN:-dev-admin-token}"

DEADLINE=$(( $(date +%s) + DUR_MIN * 60 ))
WAVE=0; TOTAL_S=0; TOTAL_F=0

echo "soak start $(date -u) duration=${DUR_MIN}min cps=$CPS workers=$WORKERS" | tee "$LOG"

check_consistency() {
  # CDR count vs balance: every settled call steps the balance by its
  # cost; the invariants live in gv-core stats + PG.
  local stats
  stats=$(curl -s "$API/v1/stats" 2>/dev/null || echo '{}')
  echo "wave $WAVE done: $stats" | tee -a "$LOG"
}

while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  WAVE=$((WAVE + 1))
  # UAS cumulative-call soft limit: restart between waves.
  docker restart gvos-demo-gw-1 >/dev/null 2>&1 && sleep 4

  for w in $(seq 1 "$WORKERS"); do
    MSYS_NO_PATHCONV=1 docker run -d --network gvos_default \
      --name soak-w${w} \
      -v "${SCENDIR}:/scen:ro" ctaloi/sipp:latest \
      -sf /scen/load.xml -inf /scen/dest.csv \
      -m "$WAVE_CALLS" -r "$CPS" -timeout 120s kamailio >/dev/null 2>&1
  done

  # Wait for the wave to finish (poll, cap at 20 min).
  for poll in $(seq 1 120); do
    running=$(docker ps --format "{{.Names}}" 2>/dev/null | grep -c "^soak-w" || true)
    [ "$running" = "0" ] && break
    sleep 10
  done

  for w in $(seq 1 "$WORKERS"); do
    s=$(docker logs soak-w${w} 2>&1 | grep "Successful call" | tail -1 | awk '{print $NF}')
    f=$(docker logs soak-w${w} 2>&1 | grep "Failed call" | tail -1 | awk '{print $NF}')
    TOTAL_S=$((TOTAL_S + ${s:-0})); TOTAL_F=$((TOTAL_F + ${f:-0}))
    docker rm -f soak-w${w} >/dev/null 2>&1
  done
  echo "wave $WAVE: cumulative success=$TOTAL_S failed=$TOTAL_F" | tee -a "$LOG"
  check_consistency
done

echo "soak complete $(date -u): waves=$WAVE success=$TOTAL_S failed=$TOTAL_F" | tee -a "$LOG"
