#!/usr/bin/env bash
# CONCURRENCY ladder (hold-time based) for the capbench compose project.
# hold60.xml keeps each call up ~60s, so concurrent = rate x duration.
# Per step: target concurrency, ramp rate, SIPp instances.
#   e.g. bash ladder2.sh "1000:20 2000:40 5000:100" [rounds]
# Quality gates mirror bench100.sh: CDR=ledger parity, dur0 share,
# answer coverage; plus system gauges (SHM, rtpengine ports in TIME_WAIT).
# Requires: compose stack up with project name `capbench`
# (GVOS_SIP_PORT=25060 GVOS_API_PORT=28081 -p capbench).
set -uo pipefail
STEPS="${1:-1000:20 2000:40 5000:100}"
ROUNDS="${2:-1}"
SCENDIR="$(cd "$(dirname "$0")" && pwd -W 2>/dev/null || pwd)"
NET=capbench_default
PSQL() { docker exec capbench-postgres-1 psql -U gvos -d gvos -tA -c "$1"; }
SHM() { docker exec capbench-kamailio-1 kamcmd core.shmmem 2>/dev/null | grep -E "^	(real_used|total)" | awk '{print $2}' | paste -sd/; }

# concurrency math: hold60 keeps a call ~60s; instances run a fixed
# total call count each, so steady concurrent ~= rate x 60s.
# STEP spec is "target_conc:instances"; rate = target/60 per instance.
echo "gvos concurrency ladder (hold60) - $(date -u +%FT%TZ) net=$NET"
printf "%-7s %-6s %-8s %-8s %-9s %-9s %-8s %-9s %-9s %-9s %-11s\n" \
  "conc" "insts" "crt" "cdr" "succ%" "led" "live_pk" "pdd_p99" "dur0%" "ans%" "shm_used"

for STEP in $STEPS; do
  C="${STEP%%:*}"; N="${STEP##*:}"
  RATE=$(( (C / 60 + N - 1) / N ))  # ceil: per-instance rate to reach C concurrent
  PER=$(( RATE * 60 ))              # each instance runs ~60s worth then exits
  for i in $(seq 1 $N); do
    MSYS_NO_PATHCONV=1 docker run -d --rm --name lad2-$C-$i --network $NET \
      -v "${SCENDIR}:/scen:ro" ctaloi/sipp:latest \
      -cid_str "%u-L2C${C}-${i}-$(date +%s)@%s" \
      -sf /scen/hold60.xml -inf /scen/dest.csv -m $PER -r $RATE -l 4000 -timeout 300s kamailio >/dev/null 2>&1
  done
  # wait for all instances of this step to finish (hold60 = 60s + tail)
  for w in $(seq 1 120); do
    RUNNING=$(docker ps -q --filter name=lad2-$C | wc -l)
    [ "$RUNNING" = "0" ] && break
    sleep 5
  done
  docker rm -f $(docker ps -aq --filter name=lad2-$C) >/dev/null 2>&1
  LIVE=$(PSQL "SELECT count(*) FROM cdr WHERE call_id LIKE '%-L2C${C}-%';")
  LED=$(PSQL "SELECT count(*) FROM balance_ledger WHERE call_id LIKE '%-L2C${C}-%';")
  P99=$(PSQL "SELECT coalesce(percentile_disc(0.99) WITHIN GROUP (ORDER BY pdd_ms), -1) FROM cdr WHERE call_id LIKE '%-L2C${C}-%';")
  DUR0=$(PSQL "SELECT count(*) FILTER (WHERE duration=0) FROM cdr WHERE call_id LIKE '%-L2C${C}-%';")
  ANS=$(PSQL "SELECT count(*) FILTER (WHERE answer_time IS NOT NULL) FROM cdr WHERE call_id LIKE '%-L2C${C}-%';")
  # live dialog peak: pull max gv-core live_calls sample from the step window
  PEAK=$(docker logs capbench-gv-core-1 --since 10m 2>&1 | grep -o 'live_calls":[0-9]*' | grep -o '[0-9]*' | sort -n | tail -1)
  [ -z "${PEAK:-}" ] && PEAK=0
  SUCC=$([ "${LIVE:-0}" -gt 0 ] && echo $(( LIVE * 100 / (N * PER) )) || echo 0)
  DUR0P=$([ "${LIVE:-0}" -gt 0 ] && echo $(( DUR0 * 100 / LIVE )) || echo 0)
  ANSP=$([ "${LIVE:-0}" -gt 0 ] && echo $(( ANS * 100 / LIVE )) || echo 0)
  printf "%-7s %-6s %-8s %-8s %-9s %-9s %-8s %-9s %-9s %-9s %-11s\n" \
    "$C" "$N" "$(( N * PER ))" "$LIVE" "${SUCC}%" "$LED" "$PEAK" "${P99}ms" "${DUR0P}%" "${ANSP}%" "$(SHM)"
  sleep 45  # cooldown: rtpengine --timeout 60 reaps ports; SHM dlg GC
done
echo done.
