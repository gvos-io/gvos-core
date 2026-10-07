#!/usr/bin/env bash
# Capacity benchmark: run the load scenario at increasing rates and
# capture SIPp statistics per step. Results to stdout.
# Usage: bash bench.sh "5 10 20 40"
set -uo pipefail

STEPS="${1:-5 10 20 40}"
SCENDIR="$(cd "$(dirname "$0")" && pwd -W 2>/dev/null || pwd)"
DEST=kamailio

echo "G-VOS capacity benchmark - $(date -u +%FT%TZ)"
echo "scenario: load.xml (Digest auth, 500ms call, UDP)"
printf "%-8s %-7s %-8s %-7s %-8s\n" "rate" "calls" "success" "failed" "setup(ms)"

for R in $STEPS; do
  N=$(( R * 10 ))   # 10 seconds of traffic per step
  echo "--- rate $R cps, $N calls ---" >&2
  RAW=$(MSYS_NO_PATHCONV=1 docker run --rm --network gvos_default \
    -v "${SCENDIR}:/scen:ro" ctaloi/sipp:latest \
    -sf /scen/load.xml -inf /scen/dest.csv -m $N -r $R -timeout 30s $DEST 2>&1)
  SUCC=$(echo "$RAW" | grep "Successful call" | tail -1 | awk '{print $NF}')
  FAIL=$(echo "$RAW" | grep "Failed call" | tail -1 | awk '{print $NF}')
  TOT=$(echo "$RAW"  | grep "Total Call created" | tail -1 | awk '{print $NF}')
  RT=$(echo "$RAW"  | grep "Response Time 1" | tail -1 | awk '{print $(NF)}')
  [ -z "$SUCC" ] && SUCC=0
  [ -z "$FAIL" ] && FAIL=0
  [ -z "$TOT" ] && TOT=0
  printf "%-8s %-7s %-8s %-7s %-8s\n" "$R" "$TOT" "$SUCC" "$FAIL" "${RT:-n/a}"
done
echo "done."
