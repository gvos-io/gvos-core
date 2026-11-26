# Changelog

All notable changes to gvos-core follow this file. Format: Keep a Changelog; versioning follows the repo tags.

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
