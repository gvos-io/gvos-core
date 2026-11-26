#!/usr/bin/env bash
# gvos one-command demo: prerequisites check, stack up, seed, smoke
# call, and print the access URLs. Works on Git Bash (Windows) and
# Linux. Usage: bash scripts/demo-up.sh
set -uo pipefail
cd "$(dirname "$0")/../deploy"

fail() { echo "FAIL: $1"; exit 1; }
echo "== gvos demo installer =="

# 1. Prerequisites.
command -v docker >/dev/null 2>&1 || fail "docker not found - install Docker Desktop / Engine first"
docker compose version >/dev/null 2>&1 || fail "docker compose plugin missing"
docker info >/dev/null 2>&1 || fail "docker daemon not running - start Docker Desktop / dockerd"

# 2. Stack up (build if needed).
echo "-- building & starting the stack (first run takes a few minutes)..."
docker compose -p gvos up -d --build 2>&1 | grep -vE "^ +(Network|Container|Volume|Image)" | tail -3

# 3. Wait for health.
echo "-- waiting for services..."
OK=down
for i in $(seq 1 30); do
  OK=$(curl -s -m 3 http://127.0.0.1:${GVOS_API_PORT:-28080}/healthz 2>/dev/null || echo down)
  [ "$OK" = "ok" ] && break
  sleep 2
done
[ "$OK" = "ok" ] || fail "gv-core did not become healthy in 60s"

# 4. Apply the schema (idempotent). The initdb hook covers a FRESH
# volume; a retained one (demo re-run, stack kept up) skips initdb, so
# run the migrations explicitly: psql errors on the already-applied
# statements are tolerated (every file is IF NOT EXISTS / guarded).
echo "-- applying migrations..."
docker exec -i gvos-postgres-1 psql -U gvos -v ON_ERROR_STOP=0 -q \
  < ../migrations/001_init.sql >/dev/null 2>&1 || true
for f in ../migrations/0*.sql; do
  docker exec -i gvos-postgres-1 psql -U gvos -q < "$f" >/dev/null 2>&1 || \
    echo "   (migration $f reported errors — expected on a re-run)"
done

# 5. Seed demo data (idempotent).
docker exec gvos-postgres-1 psql -U gvos -c "
  UPDATE accounts SET credit = 10000 WHERE username='1000';" >/dev/null
docker restart gvos-gv-core-1 >/dev/null 2>&1
sleep 5

# 6. Smoke test: one real SIP call.
echo "-- smoke call (authed INVITE through the billing path)..."
docker restart gvos-demo-gw-1 kamailio >/dev/null 2>&1
sleep 6
SIPP_OUT=$(MSYS_NO_PATHCONV=1 docker run --rm --network gvos_default \
  -v "$(cd ../test/sipp && pwd -W 2>/dev/null || pwd):/scen:ro" ctaloi/sipp:latest \
  -sf /scen/uac_auth.xml -inf /scen/dest.csv -m 1 kamailio 2>&1 || true)
if echo "$SIPP_OUT" | grep -q "Successful call"; then
  echo "-- smoke call: OK (1/1)"
else
  echo "-- smoke call: did not complete (check 'docker logs gvos-kamailio-1')"
fi

# 7. Report.
STATS=$(curl -s http://127.0.0.1:${GVOS_API_PORT:-28080}/v1/stats)
echo
echo "=============================================="
echo " gvos demo is up."
echo "----------------------------------------------"
echo " Admin console : http://127.0.0.1:${GVOS_API_PORT:-28080}/ui"
echo " Admin token   : dev-admin-token (legacy; deprecated once a key"
echo "                 is set - create one: POST /v1/admin/keys)"
echo " SIP endpoint  : udp/tcp 127.0.0.1:5060"
echo " SIP over WSS  : wss://127.0.0.1:4444 (self-signed)"
echo " API health    : http://127.0.0.1:${GVOS_API_PORT:-28080}/healthz"
echo " Demo account  : user 1000 / password demo1000"
echo " Stats         : $STATS"
echo "=============================================="
echo " Tear down     : docker compose -p gvos down"
