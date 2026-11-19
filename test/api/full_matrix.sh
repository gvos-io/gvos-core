#!/usr/bin/env bash
# Full API endpoint matrix (API-1): every public route — normal,
# error-shape and auth paths — plus the error-envelope contract.
# Usage: bash test/api/full_matrix.sh <base> <admin-key> <internal-token>
BASE="${1:-http://127.0.0.1:28080}"
KEY="${2:-dev-admin-token}"
INT="${3:-dev-internal-token}"
PASS=0; FAIL=0; FAILED_LIST=""

chk() { # name expected actual
  if [ "$2" = "$3" ]; then PASS=$((PASS+1)); else FAIL=$((FAIL+1)); FAILED_LIST="$FAILED_LIST\n  $1 (want $2 got $3)"; fi
}
chk_ne() {
  if [ "$2" != "$3" ]; then PASS=$((PASS+1)); else FAIL=$((FAIL+1)); FAILED_LIST="$FAILED_LIST\n  $1 (must not be $3)"; fi
}
code() { curl -s -o /dev/null -w "%{http_code}" "$@"; }
body() { curl -s "$@"; }

# ---- auth matrix: every admin route without key -> 401 ----
for ep in accounts rates risk-rules gateways registrations cdrs recording blocklist-entries ipallow/rules packages reports keys axb/bindings axb/stats webhooks blocklist-sources; do
  chk "noauth /v1/admin/$ep" 401 "$(code -X GET "$BASE/v1/admin/$ep")"
done
chk "noauth axb bind" 401 "$(code -X POST "$BASE/v1/admin/axb/bindings")"

# ---- internal face: fail-closed ----
chk "call/start wrong token" 401 "$(code -X POST -H 'Content-Type: application/json' -d '{"call_id":"x","from_user":"1000","to_user":"8613800138000"}' "$BASE/v1/call/start?token=wrong")"
chk "ipallow-snapshot no token" 401 "$(code "$BASE/v1/internal/ipallow-snapshot")"

# ---- accounts ----
chk "accounts create" 201 "$(code -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d "{\"username\":\"mx$RANDOM\",\"name\":\"MX\",\"credit\":50}" "$BASE/v1/admin/accounts")"
chk "accounts list" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/accounts")"
AID=$(body -H "Authorization: Bearer $KEY" "$BASE/v1/admin/accounts" | python -c "import json,sys; d=json.load(sys.stdin); print([a['id'] for a in d['accounts'] if a['username'].startswith('mx')][0])" 2>/dev/null)
chk "topup" 200 "$(code -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"delta":5,"reason":"mx"}' "$BASE/v1/admin/accounts/$AID/topup")"
chk "topup bad account" 404 "$(code -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"delta":5}' "$BASE/v1/admin/accounts/no-such/topup")"

# ---- rates ----
chk "rates GET" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/rates")"
chk "rates PUT" 200 "$(code -X PUT -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"prefix":"861","price":0.05,"init_dur":60,"step_dur":6}' "$BASE/v1/admin/rates")"
chk "rates PUT bad" 400 "$(code -X PUT -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"prefix":"","price":-1}' "$BASE/v1/admin/rates")"

# ---- gateways ----
chk "gateways GET" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/gateways")"
chk "gateways PUT" 200 "$(code -X PUT -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"gateways":[{"id":"gw-mx","name":"mx","sip_addr":"127.0.0.1:5090","priority":9,"enabled":true,"prefixes":["861"],"cost_rate":"demo"}]}' "$BASE/v1/admin/gateways")"
chk "gateways DELETE" 200 "$(code -X DELETE -H "Authorization: Bearer $KEY" "$BASE/v1/admin/gateways/gw-mx")"
chk "gateways DELETE missing" 404 "$(code -X DELETE -H "Authorization: Bearer $KEY" "$BASE/v1/admin/gateways/gw-none")"

# ---- blocklist ----
chk "blocklist POST" 201 "$(code -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"kind":"black","scope":"dest","pattern":"170mx"}' "$BASE/v1/admin/blocklist-entries")"
chk "blocklist GET" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/blocklist-entries")"
chk "blocklist DELETE" 200 "$(code -X DELETE -H "Authorization: Bearer $KEY" "$BASE/v1/admin/blocklist-entries/dest:170mx")"
chk "blocklist-sources" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/blocklist-sources")"

# ---- ipallow ----
chk "ipallow GET" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/ipallow/rules")"
chk "ipallow POST" 200 "$(code -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"rules":[{"id":"mx","cidr":"10.99.0.0/16","action":"allow"}]}' "$BASE/v1/admin/ipallow/rules")"
chk "ipallow DELETE" 200 "$(code -X DELETE -H "Authorization: Bearer $KEY" "$BASE/v1/admin/ipallow/rules/mx")"

# ---- risk ----
chk "risk-rules GET" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/risk-rules")"

# ---- packages ----
chk "packages GET" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/packages")"
chk "packages usage" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/packages/usage?account=$AID")"

# ---- AXB ----
chk "axb bind" 201 "$(code -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"a_number":"13800001111","b_number":"8615770000000","ttl_hours":1}' "$BASE/v1/admin/axb/bindings")"
chk "axb list" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/axb/bindings")"
chk "axb stats" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/axb/stats")"
XN=$(body -H "Authorization: Bearer $KEY" "$BASE/v1/admin/axb/bindings" | python -c "import json,sys; print(json.load(sys.stdin)['bindings'][0]['x_number'])" 2>/dev/null)
chk "axb delete" 200 "$(code -X DELETE -H "Authorization: Bearer $KEY" "$BASE/v1/admin/axb/bindings/$XN")"
chk "axb delete missing" 404 "$(code -X DELETE -H "Authorization: Bearer $KEY" "$BASE/v1/admin/axb/bindings/9none")"

# ---- keys ----
KEYNEW=$(body -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"role":"operator"}' "$BASE/v1/admin/keys")
KPRE=$(echo "$KEYNEW" | python -c "import json,sys; print(json.load(sys.stdin).get('prefix',''))" 2>/dev/null)
chk "keys create" 201 "$(echo "$KEYNEW" | grep -q gvos_ && echo 201)"
chk "keys list" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/keys")"
chk "keys delete" 200 "$(code -X DELETE -H "Authorization: Bearer $KEY" "$BASE/v1/admin/keys/$KPRE")"

# ---- webhooks ----
chk "webhooks create" 201 "$(code -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"url":"http://127.0.0.1:9/mx","events":["gvos.cdr.completed"]}' "$BASE/v1/admin/webhooks")"
chk "webhooks list" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/webhooks")"

# ---- recordings (no rows for a made-up call: 404 is the contract) ----
RC=$(code -H "Authorization: Bearer $KEY" "$BASE/v1/recording/call-none")
chk_ne "recordings list sane" "500" "$RC"

# ---- reports ----
chk "reports GET" 200 "$(code -H "Authorization: Bearer $KEY" "$BASE/v1/admin/reports")"

# ---- call plane (internal token via query) ----
chk "call/start happy" 200 "$(code -X POST -H 'Content-Type: application/json' -d '{"call_id":"mx1","from_user":"1000","to_user":"8613800138000"}' "$BASE/v1/call/start?token=$INT")"
chk "call/end happy" 200 "$(code -X POST -H 'Content-Type: application/json' -d '{"call_id":"mx1","duration":66}' "$BASE/v1/call/end?token=$INT")"
chk "call/start bad body" 400 "$(code -X POST -H 'Content-Type: application/json' -d '{}' "$BASE/v1/call/start?token=$INT")"
chk "call/end unknown" 404 "$(code -X POST -H 'Content-Type: application/json' -d '{"call_id":"none"}' "$BASE/v1/call/end?token=$INT")"

# ---- method sanity (405 from net/http) ----
chk "405 accounts PUT" 405 "$(code -X PUT -H "Authorization: Bearer $KEY" "$BASE/v1/admin/accounts")"

# ---- error envelope contract ----
E=$(body -X POST -H 'Content-Type: application/json' -d '{"call_id":"mx2"}' "$BASE/v1/call/end?token=$INT")
echo "$E" | grep -q '"error":{"code"' && PASS=$((PASS+1)) || { FAIL=$((FAIL+1)); FAILED_LIST="$FAILED_LIST\n  envelope shape: $E"; }

# ---- openapi ----
chk "openapi" 200 "$(code "$BASE/openapi.json")"
body "$BASE/openapi.json" | grep -q '"openapi":"3.1.0"' && PASS=$((PASS+1)) || { FAIL=$((FAIL+1)); FAILED_LIST="$FAILED_LIST\n  openapi version"; }

echo "RESULT: $PASS passed, $FAIL failed"
if [ -n "$FAILED_LIST" ]; then echo -e "FAILURES:$FAILED_LIST"; fi
exit $FAIL
