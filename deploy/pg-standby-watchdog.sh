#!/bin/sh
# Standby promotion watchdog (PGFO-2, the lightweight failover half of
# the dual-host hot-standby): probe the primary on an interval; after
# N consecutive misses, promote THIS standby. The gv-core DSN lists
# both hosts with target_session_attrs=read-write, so application
# connections land on whichever node is primary without a restart.
#
# This is the pragmatic compose-tier failover. Patroni (consensus,
# RPO-controlled elections) remains the production recommendation —
# recorded in 02-architecture §7.
PROBE_HOST="${PG_WATCH_PRIMARY:-pg-primary}"
PROBE_INTERVAL="${PG_WATCH_INTERVAL:-5}"
MISSES_TO_PROMOTE="${PG_WATCH_MISSES:-3}"
PGDATA_DIR="${PGDATA:-/var/lib/postgresql/data}"

misses=0
while true; do
  if pg_isready -h "$PROBE_HOST" -p 5432 -q -t 3; then
    misses=0
  else
    misses=$((misses + 1))
    echo "watchdog: primary probe failed ($misses/$MISSES_TO_PROMOTE)"
    if [ "$misses" -ge "$MISSES_TO_PROMOTE" ]; then
      echo "watchdog: promoting standby to primary"
      pg_ctl promote -D "$PGDATA_DIR" && echo "watchdog: promoted; watchdog exits"
      exit 0
    fi
  fi
  sleep "$PROBE_INTERVAL"
done
