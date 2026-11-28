# Changelog

All notable changes to gvos-core follow this file. Format: Keep a Changelog; versioning follows the repo tags.

## [Unreleased]

### Added — M10 tenant write plane (authorization ruling: tenant funds+provisions, agent read-only, 2026-11-28)
- **Tenant-plane transfer semantics**: `POST /v1/tenant/accounts/{id}/topup` moves credit from the tenant root DOWN to a child (positive delta only — clawback is an admin action; both sides journaled as transfer-out/transfer-in in one PG transaction with the affordability check under row lock). A reseller cannot conjure credit; the ledger conservation invariant holds.
- **Tenant child provisioning**: `POST /v1/tenant/accounts` — username/name/rate_group (defaults to the tenant's own book)/initial_credit (allocated via the same transfer primitive, never from nothing); one-time SIP password response mirrors the admin create; agent role is read-only (403); subtree escape attempts answer a bare 404.
- `store.TransferCredit` (PG, single tx: row-locked debit, credit, dual ledger rows) and the idempotency face covers both new routes.

### Added — M11 design (approved for planning, 2026-11-28)
- `docs/click-to-call-design.md`: origination API design (F1 two-way callback → F2 AI outbound via the reserved media_agent destination → F3 campaigns); `POST /v1/calls` resource model, call state machine + call.state contract event, originate control-plane instruction with an outboard UAC microservice option, display-number allowlist and per-account origination CPS compliance guards.

### Added — P1 (gap analysis M6/M7/M8/M9, 2026-11-28)
- **Account lifecycle**: `PATCH /v1/admin/accounts/{id}` (name, rate_group, postpaid, max_debt, max_concurrent, max_cps, low_balance — credit deliberately excluded: balance moves only through topup/settlement so the ledger stays the single money story) and `DELETE /v1/admin/accounts/{id}` (soft delete via `deleted_at`; refused 409 HAS_CHILDREN while live children exist). Peer replicas apply the bus snapshot incl. an op=delete branch. Disabling an account remains future work — the schema/admission path has no enabled flag yet.
- **Audit-trail read**: `GET /v1/admin/audit?entity=&entity_id=&tenant_id=&limit=&cursor=` — the audit_log table had been written on every mutation since migration 001 with no reader. Keyset pagination on id; PG-mode only (memory mode answers 503 UNAVAILABLE rather than a silent empty trail).
- **Ledger read**: `GET /v1/admin/accounts/{id}/ledger` — balance_ledger movements (delta, post-movement balance, reason) with the same pagination contract. PG-mode only.
- **Webhook events 4/4** (roadmap Phase D's planned set is now complete): `account.low_balance` (threshold is a new account field `low_balance`, PATCH-able; fires at most once per hour per account, re-armed by topup; migration 014 adds the column), `risk.blocked` (fraud-engine block at call admission), `package.exhausted` (allowance crossing detected exactly via the new `RateResult.ExhaustedNow` — the old `FreeSecs==0 && BillableSecs==0` test fired on unrelated no-charge shapes). The subscribable-enum validation now lives in one place (`webhook.ContractSubjects`) instead of two drifting copies.
- **Webhook PATCH** `PATCH /v1/admin/webhooks/{id}` (url/events/active; the secret is deliberately immutable — delete + re-create is the rotation path) and **tenant webhook DELETE** `DELETE /v1/tenant/webhooks/{id}` (ownership enforced in the SQL WHERE / mem-sub tenant field; foreign endpoints 404).
- **Package unassign**: `PUT /v1/admin/packages {"unassign_account":...}` (same username-resolution rule as assign; bus op=unassign applied by replicas) and usage endpoint accepts the SIP username.
- **Rate delete**: `DELETE /v1/admin/rates/{prefix}` — PG retires rows via `ended_at` (history kept); memory mode drops from the list; broadcast entity=rate.

### Changed — P1 hardening
- Gateway full-set PUT/DELETE are serialized under a mutex — concurrent writers used to lose updates.
- Registrations and DID lists gained `?limit=&cursor=` server-side paging with stable ordering (Entries/List were unordered map walks) and a `total` field.
- Webhook delivery log gained keyset pagination (`next_cursor` on id) — previously clamped to the first page with no way forward.
- Tenant account-set resolution (the O(N×depth) tree walk on every request across six tenant routes) is cached for 5s, invalidated on account create/delete.
- Tenant stats: `today_spend`/`today_cdrs` read the `usage_daily` rollup in PG mode (the 10k ring silently truncated before; source is reported via `spend_source`), and the memory-mode scan snapshots the ring under the write lock and computes outside it — the full-ring walk under `cdrsMu` used to block `storeCdr`, the billing write path.

### Fixed — P1
- `UpdateAccountPatch` dropped its argument slice at the Exec call site (statements referenced $1..$N while pgx received none — caught by the PG-gated lifecycle test).
- `TestResetRuntimeState` (PG test helper) orders ledger deletion before test-account deletion (FK balance_ledger→accounts) so the PG suite passes on consecutive runs against one throwaway schema.

### Added
- `docs/api-gap-analysis.md`: five-pass API gap analysis (data-model × API, capability × API, per-route audit, roadmap/competitor commitments, cross-cutting conventions) with P0/P1/P2 missing-API and refactoring lists, all with file:line evidence, plus core/console ownership rulings per ADR-0002/0006.
- **CDR history query (PG authority)**: `GET /v1/admin/cdrs` and `GET /v1/tenant/cdrs` now read full history in PG mode via keyset pagination on `(start_time, call_id)` — the 10k memory ring stays the recent view in memory mode. Both faces gained server-side filters `account_id`/`dest`/`call_id`/`from`/`to` (previously parsed nowhere; console filters worked only against its mock). Tenant face rejects an out-of-scope `account_id` with 403. Cursors on PG are opaque base64; both faces share `items` + `next_cursor`.
- **Unified list envelope**: every admin/tenant list response now carries `items` + `count` (keys, risk-rules, registrations, gateways, dids, webhooks, axb-bindings, blocklist entries, tenant accounts/webhooks/usage/reports; tenant CDRs add `next_cursor` alongside the legacy `cursor`/`has_more`). The console read `items` everywhere — three classes of blank pages under the real core are fixed by this alone.
- **Single-account read**: `GET /v1/admin/accounts/{id}` (id or username; 404 envelope) — the console detail page used to pull the whole list and match client-side.
- **Stats counts**: `GET /v1/stats` now reports `counts:{accounts, gateways, registrations, tenants}` for the ops dashboard.
- **Blocklist listing**: `GET /v1/admin/blocklist-entries` returns the real entries (it used to return only a count) and `DELETE .../{id}` accepts `[black|white:]<src|dest>:<pattern>` so whitelist entries can finally be lifted (kind was hardcoded `black`).
- `migrations/014_api_keys_idempotency.sql`: `api_keys` (roadmap Phase B DDL), `idempotency_keys` (key + stored first response + TTL), `axb_bindings.ref` (stable binding ids across restarts).
- **API keys persist (migration 014)**: create/revoke write through to PG; boot loads the table into the in-memory acl store (hot path stays zero-IO); replicas converge via cfg.reload `keys` (re-pull, no key material on the bus); bootstrap mint is idempotent. Before this, a restart invalidated every issued key and replicas disagreed about auth.
- **AXB bindings persist (migration 010 table)**: bind/revoke write through, boot restores live bindings, cfg.reload `axb` snapshot converges replicas, hourly purge drops dead rows. A restart used to drop every binding and X-number dials 404'd until re-created.
- **Idempotency hardening**: registry keys are namespaced `"<METHOD> <path>|<client key>"` (the flat namespace cross-replayed responses across routes); PG mode stores reservations + responses in `idempotency_keys` with an hourly 24h purge, so replay survives restarts and works across replicas; memory-mode replays now carry the original content type; `PUT /v1/admin/packages` and `POST /v1/admin/keys` join the idempotent face.

### Changed
- **Rate limiter only guards the admin/tenant planes** (`/v1/admin/*`, `/v1/tenant/*`): it used to wrap the whole mux, clamping the signaling plane to ~20 rps/IP and shadowing the billing gate's own 429s. Bucket keying now hashes the bearer token from the header directly (the acl identity is injected only inside handlers, after the middleware runs — the per-key buckets were dead code before).
- `pg.LoadRates` split into `LoadRateRows` + table wrapper; `NewPGServer` backfills `rateList` so `GET /v1/admin/rates` shows the real price book in PG mode (it was permanently empty there) and `rebuildRates` no longer rebuilds from nil.
- DID PUT: omitted `enabled` now defaults to true — a DID registered without it used to land disabled (zero value) and silently never routed. Explicit `enabled:false` unchanged.
- Memory-mode webhook subscriptions now actually live in the dispatcher (create returned 201 while the subscription evaporated: list empty, nothing ever delivered); memory-mode webhook delete 404s on unknown ids instead of silently 200ing.

### Fixed
- `GET /v1/admin/recording` returns the toggle state (the OpenAPI-promised query half used to fall through to `requirePOST` and always 405); the toggle writes under `cfgMu`.
- `DELETE /v1/admin/gateways/{id}` persists via the `gateways` settings key — deleted gateways used to resurrect on restart/replica reload; the PUT-branch 401/405 double-write is fixed alongside.
- AXB `ttl_hours` NaN no longer slips past the default (NaN comparisons) into `ExpireAt`.

### Removed
- `/ui` deprecated spike admin console deleted (ADR-0006): routes `/ui`, `/ui/`, handler and the embedded `internal/api/ui.go` HTML (243 lines) are gone. Human-facing UI lives exclusively in gvos-console; core now serves only the JSON API, `/v1/stream/`, `/openapi.json`, `/healthz` and `/metrics`.
- `GET /v1/stats` no longer reports `caller_bal` (hardcoded demo account "1000"; dead in PG mode, consumed by nobody).
- **`/ui` deprecated spike admin console deleted** (ADR-0006): routes `/ui`, `/ui/`, handler and the embedded `internal/api/ui.go` HTML (243 lines) are gone. Human-facing UI lives exclusively in gvos-console; core now serves only the JSON API, `/v1/stream/`, `/openapi.json`, `/healthz` and `/metrics`.

## [0.5.0] — 2026-11-26

### Capacity (docs/bench/capacity-ladder-2026-11-26.md)
- **Single-node 20,000 concurrent calls, full stack, verified**: seven-step hold60 ladder 1k→2k→5k→9k→13k→17k→20k on one dev-machine compose stack; every step CDR=ledger exact to the cent (80,468 rows this session); PDD p99 65ms at 20k; Kamailio SHM peak 51MB/64MB. Matches the VOS5000 single-machine nominal (2万线) for signaling+billing; media-flow saturation remains B-line.
- `test/sipp/ladder2.sh`: concurrency-ladder runner with bench100-style quality gates.

### Added — DID inbound termination (callee-billed) (docs/did-inbound-design.md)
- `migrations/013_did.sql`: `dids` registry (number → owner account/destination type/destination) + `cdr.direction` ('in'/'out') + backfilled `gateway_id` write.
- Control plane: inbound bill-lock locks the **DID-owning account** (pipecat O1 resolved: callee-billed); inbound tariff = the owner's own rate group (zero rate schema change); src-scope screening of the carrier caller-ID; `media_agent` destination type is the reserved AI voice-agent seam (answered 501 — never bills what it cannot route).
- Admin API `POST/GET/DELETE /v1/admin/dids` + snapshot `GET /v1/internal/did-snapshot`; cfg.reload broadcast entity "did" (60s rebroadcast + 5s edge poll).
- Edge: `did` htable + `DID_INBOUND` branch (trunk INVITEs skip Digest — CIDR admission already gated), BILL_LOCK body carries `direction:inbound`; reply handling terminates `user` destinations via `lookup("location")` and `external` second legs via the control-plane pre-picked `gw_id`.
- E2E verified: carrier INVITE (no credentials) → registered user rings, answers, callee billed; CDR `direction='in'`; 3/3 calls, ledger parity exact.

### Fixed — media plane (B-line SRTP / transcode / recording, 2026-11-26)
- **`GVOS_MEDIA_OFF` empty string silently disabled media** (`#!trydefenv` defines on existence): entrypoint now `unset`s empty values. Evidence: rtpengine offer/answer=0 since bench10.
- **`generate SDES` / `replace-session-connection` are not valid rtpengine mr26 flags** (unknown→ignored). Offer/answer use `replace-origin` + `DTLS=off` (+ egress PCMU policy below).
- **`rtpengine_manage` after `t_continue()` runs as FAILURE_ROUTE** (module docs): treated as delete / skipped body rewrite. Offer moved to `branch_route[RTP_OFFER]`; answer hooked in `onreply_route`.
- SRTP e2e **PASS**: pjsua → 200 `RTP/SAVP` + `a=crypto` at rtpengine, `SRTP status: Active`.
- **Transcode baseline PASS (G722↔PCMU)**: offer flags `codec-mask-all codec-transcode-PCMU` (egress PCMU); answer restores offerer codec. Stock `gvos-pjsua` lacks runtime Opus (Dockerfile now pulls `libopus-dev` for rebuild); iLBC is not in rtpengine's transcoder set. Script: `test/srtp/run-pjsua-transcode.sh`.
- **Recording spool PASS (userspace)**: settings `recording.enabled=true` → `start_recording()` → eth pcap under `/recording/pcaps` + closed metadata under `/recording/metadata` (finalize via media `--timeout`; BYE-path `rtpengine_delete` only when `meta=>rec` is set). Keep `--recording-method pcap` — `proc` needs `xt_RTPENGINE` ("kernel table not open" in-container).
- **Recording daemon MP3**: compose `--profile recording` builds/runs; consumes `/proc/rtpengine/...` so **blocked without kernel module**. LocalFS/recpipe still watches `/output` for when host has kernel offload. Do not put `#` comments between entrypoint `\` continuations (they ate `--recording-*` flags).
- `test/srtp/run-pjsua-srtp.sh` corrected (reg-first stdin, realm/password/ports/DID pitfalls).

### Fixed — production hardening
- **F-7**: gv-core no longer Fatal-exits when PostgreSQL is unreachable at boot — capped-backoff retry (3s→30s, 20 attempts); the pool itself reconnected post-New all along.
- **Edge NATS authentication was broken end-to-end** (silent): compose never passed `GVOS_NATS_USER/PASSWORD` to kamailio, and the entrypoint rendered a `nats_user` modparam that this nats module build does not have (startup-fatal). Credentials now ride the URL userinfo (`nats://user:pass@host`, libnats-native).
- **AXB `rewrite_dest` was parked, never applied** (the "已接" claim in the AXB design doc): BILL_LOCK_REPLY now substitutes `$rU` for real — AXB and inbound-DID routing both depend on it.
- **Bill-lock reply envelope was never un-nested**: edge jansson paths addressed `{code, body:{...}}` as flat — `direction`/`rewrite_dest`/`gw_id`/`record` regexes missed or misread. Envelope flattened once (`blr_body`), regexes tolerate JSON spacing.
- **In-dialog ACK/BYE for user-terminated inbound calls** relayed to the outbound gateway (or self-looped on the DID R-URI): GW_CONTACT now rewrites the R-URI to the registered username and relays to the contact; 481 when offline.
- `deploy/kamailio-entrypoint.sh`: CRLF line endings made the entrypoint unexecutable after a Windows checkout (`exec: no such file or directory`).
- `deploy/k8s/05-kamailio.yaml`: `GVOS_CORE_HOST`/`GVOS_INTERNAL_TOKEN` were missing — the HTTP pollers fail-looped on `http://:8080/...` (observed on the live dev stack); `03-gv-core.yaml` DSN now takes `gvos-secrets/db-dsn` with an explicit dev fallback env.

### Added — security/ops
- **Admin/tenant API rate limiting** (`internal/api/ratelimit.go`): per-key-or-IP token bucket (default 100 burst / 20 rps; `GVOS_RATE_BURST`/`GVOS_RATE_RPS`, 0 disables), 429 + Retry-After; signaling/internal planes deliberately unlimited. Pentest leftover 1 closed.
- **Dev-credential telemetry**: `gvos_dev_credentials_total` counts authentications via `dev-internal-token` (signaling face) and the legacy admin constant; Prometheus alert `GvosDevCredentialsInUse` (critical) fires on any use.
- gvos-console: CI workflow (api: vet+test+build; web: npm ci + vue-tsc+vite build) — commands verified green locally.
- Dialog hash_size 16384 slots (20k-dialog benchmark headroom).

### Docs
- `docs/did-inbound-design.md` (new); `docs/02-architecture.md` F4 row corrected to ✅ (code had shipped `requireInternal` — the doc was stale); capacity ladder report under `docs/bench/`.
