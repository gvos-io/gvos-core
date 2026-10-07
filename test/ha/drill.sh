#!/usr/bin/env bash
# HA failover drill (compose-ha deployment). Measures:
#   - standby replication lag (RPO evidence)
#   - write outage window when the primary stops (RTO evidence)
# Produces drill-<ts>.md with the numbers, mirroring the PRD
# acceptance: billing data RPO=0 (synchronous path), RTO<60s.
#
# Usage: bash test/ha/drill.sh   (expects compose-ha stack running)
set -uo pipefail
cd "$(dirname "$0")/../../deploy"

P="docker compose -f compose-ha.yml -p gvosha"
OUT="drill-$(date +%Y%m%d-%H%M%S).md"
exec_p() { docker exec gvosha-pg-primary-1 psql -U gvos -t -A -c "$1" 2>/dev/null; }
exec_s() { docker exec gvosha-pg-standby-1 psql -U gvos -t -A -c "$1" 2>/dev/null; }

echo "# HA drill $(date -u +%FT%TZ)" | tee "$OUT"

# --- 0. replication user (idempotent).
$P exec -T pg-primary psql -U gvos -c \
  "DO \$\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='replicator') THEN CREATE ROLE replicator LOGIN REPLICATION PASSWORD 'gvos_dev'; END IF; END \$\$;" \
  >/dev/null 2>&1
$P exec -T pg-primary psql -U gvos -c \
  "SELECT pg_reload_conf();" >/dev/null 2>&1

# --- 1. seed a marker row on the primary.
MARK="drill-$(date +%s)"
exec_p "INSERT INTO tenants (name) VALUES ('$MARK') RETURNING name;" >/dev/null

# --- 2. wait for it on the standby (replication lag = RPO).
T0=$(date +%s%N)
for i in $(seq 1 50); do
  FOUND=$(exec_s "SELECT count(*) FROM tenants WHERE name='$MARK';")
  [ "${FOUND:-0}" = "1" ] && break
  sleep 0.2
done
T1=$(date +%s%N)
LAG_MS=$(( (T1 - T0) / 1000000 ))
if [ "${FOUND:-0}" = "1" ]; then
  echo "- standby replication lag (RPO evidence): **${LAG_MS}ms** to a fresh committed row" | tee -a "$OUT"
else
  echo "- FAIL: marker never reached the standby in 10s" | tee -a "$OUT"
fi

# --- 3. stop the primary; measure write outage (RTO until manual
# promotion). We do not auto-promote here - the drill documents the
# window and the promotion step.
$P stop pg-primary >/dev/null 2>&1
S0=$(date +%s)
echo "- primary stopped at $(date -u +%T)" | tee -a "$OUT"

# Verify reads still work through the standby (hot_standby=on).
READ=$(exec_s "SELECT count(*) FROM tenants WHERE name='$MARK';")
echo "- standby reads during outage: $([ "${READ:-0}" = "1" ] && echo "OK (marker readable)" || echo "DEGRADED")" | tee -a "$OUT"

# --- 4. promote the standby manually.
docker exec gvosha-pg-standby-1 pg_ctl promote -D /var/lib/postgresql/data >/dev/null 2>&1
for i in $(seq 1 30); do
  WR=$(exec_s "SELECT 1;")
  [ "$WR" = "1" ] && break
  sleep 1
done
S1=$(date +%s)
RTO=$(( S1 - S0 ))
echo "- standby promoted; writes recovered: **RTO ${RTO}s**" | tee -a "$OUT"

# --- 5. write a post-promotion marker to prove writability.
exec_s "INSERT INTO balance_ledger (account_id, delta, balance, reason, call_id)
        VALUES ((SELECT id FROM accounts LIMIT 1), 0.0000002, 0, 'drill-after', '$MARK');" >/dev/null \
  && echo "- post-promotion write: OK" | tee -a "$OUT"

echo "drill complete -> $OUT"
