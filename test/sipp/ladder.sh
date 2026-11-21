#!/usr/bin/env bash
# CPS capacity ladder with per-step aggregate load spread over N
# parallel SIPp instances (single instances watchdog-stall at ~200CPS
# on this host). Per step: rate x instances, m calls per instance.
# Usage: bash ladder.sh "150:2 200:2 250:3 300:3" [calls-per-instance]
set -uo pipefail
STEPS="${1:-150:2 200:2 250:3 300:3}"
PER="${2:-3000}"
SCENDIR="$(cd "$(dirname "$0")" && pwd -W 2>/dev/null || pwd)"
PSQL() { docker exec gvos-postgres-1 psql -U gvos -tA -c "$1"; }

echo "G-VOS CPS ladder - $(date -u +%FT%TZ)"
printf "%-9s %-6s %-8s %-8s %-9s %-9s %-9s %-8s %-8s\n" "rate" "insts" "attempt" "cdr" "succ%" "pdd_p99" "pdd_max" "failures" "quality"

for STEP in $STEPS; do
  R="${STEP%%:*}"; N="${STEP##*:}"
  CIDFMT="%u-LAD${R}-$(date +%s)@%s"
  for i in $(seq 1 $N); do
    MSYS_NO_PATHCONV=1 docker run -d --rm --name lad-$R-$i --network gvos_default \
      -v "${SCENDIR}:/scen:ro" ctaloi/sipp:latest \
      -cid_str "%u-S${R}-${i}-$(date +%s)@%s" \
      -sf /scen/load.xml -inf /scen/dest.csv -m $PER -r $(( R / N )) -l 4000 -timeout 120s kamailio >/dev/null 2>&1
  done
  # wait for all to exit
  for i in $(seq 1 60); do
    RUNNING=$(docker ps -q --filter name=lad-$R | wc -l)
    [ "$RUNNING" = "0" ] && break
    sleep 5
  done
  sleep 15  # CDR drain
  M=$PER; TOT=$(( M * N ))
  CNT=$(PSQL "SELECT count(*) FROM cdr WHERE call_id LIKE '%-S${R}-%';")
  ANS=$(PSQL "SELECT count(*) FILTER (WHERE answer_time IS NOT NULL) FROM cdr WHERE call_id LIKE '%-S${R}-%';")
  LED=$(PSQL "SELECT count(*) FROM balance_ledger WHERE call_id LIKE '%-S${R}-%';")
  P99=$(PSQL "SELECT coalesce(percentile_disc(0.99) WITHIN GROUP (ORDER BY pdd_ms), -1) FROM cdr WHERE call_id LIKE '%-S${R}-%';")
  PMAX=$(PSQL "SELECT coalesce(max(pdd_ms), -1) FROM cdr WHERE call_id LIKE '%-S${R}-%';")
  SUCCPCT=$([ "${CNT:-0}" -gt 0 ] && echo $(( CNT * 100 / TOT )) || echo 0)
  # failure evidence: kamailio error lines during this step
  ERR=$(docker logs gvos-kamailio-1 --since 6m 2>&1 | grep -cE "no routable gateway|resuming the processing|locked out" || true)
  printf "%-9s %-6s %-8s %-8s %-9s %-9s %-9s %-8s %-8s\n" \
    "$R" "$N" "$TOT" "${CNT:-0}" "${SUCCPCT}%" "${P99}ms" "${PMAX}ms" "$ERR" "ledger=${LED}/${CNT}"
  docker rm -f $(docker ps -aq --filter name=lad-$R) >/dev/null 2>&1
  sleep 30  # cooldown between steps (SIPp watchdog + host CPU settle)
done
echo done.
