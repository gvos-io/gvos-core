-- 014: api_keys persistence + idempotency store + axb binding ref
-- API keys survive restarts and replicas (roadmap Phase B debt):
-- sha256 hash at rest, prefix for display, tenant+role binding. The
-- hot auth path still reads the process-local acl.Store; this table
-- is the durable source loaded at boot and written through on
-- create/revoke (multi-replica convergence via cfg.reload "keys").
--
-- Idempotency-Key registry for money routes: the first response is
-- stored (status + body + content_type) and replayed verbatim within
-- the TTL. Keys are namespaced "<METHOD> <route>|<client key>" so the
-- same client key on two routes cannot cross-replay. An hourly pass
-- deletes rows older than the TTL.
--
-- axb_bindings.ref keeps the process-local binding id (axb-N) stable
-- across restarts: bindings restored from PG keep the id their
-- creators saw, so DELETE /v1/admin/axb/bindings/{id} keeps working.

CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    role TEXT NOT NULL CHECK (role IN ('admin','operator','tenant','agent')),
    key_hash TEXT NOT NULL UNIQUE,     -- sha256 hex of the plaintext key
    prefix TEXT NOT NULL,              -- display form: gvos_admin_ab12
    created_by TEXT NOT NULL DEFAULT '',
    revoked_at TIMESTAMPTZ,
    last_used TIMESTAMPTZ
);
CREATE INDEX idx_api_keys_live ON api_keys(revoked_at) WHERE revoked_at IS NULL;

CREATE TABLE idempotency_keys (
    key TEXT PRIMARY KEY,              -- "<METHOD> <route>|<Idempotency-Key>"
    status INT NOT NULL,               -- -1 while the first response is in flight
    body TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT 'application/json',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_idem_created ON idempotency_keys(created_at);

ALTER TABLE axb_bindings ADD COLUMN IF NOT EXISTS ref TEXT UNIQUE;

-- account.low_balance webhook threshold (gap analysis M8): fires as
-- the balance first drops to/below the threshold; 0 = off.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS low_balance_threshold NUMERIC(18,7) NOT NULL DEFAULT 0;
