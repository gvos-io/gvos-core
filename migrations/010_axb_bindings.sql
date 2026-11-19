-- 010: AXB privacy-number bindings (VOS AXB module equivalent).
-- A (caller) dials X (virtual number) -> the platform rewrites the
-- destination to B. B never sees A's real number and vice versa.
-- Bindings are time-boxed (expire_at) and per-tenant.

CREATE TABLE axb_bindings (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    x_number   TEXT NOT NULL UNIQUE,          -- virtual number (pool-generated)
    a_number   TEXT NOT NULL,                 -- caller real number
    b_number   TEXT NOT NULL,                 -- callee real number
    active     BOOL NOT NULL DEFAULT true,
    expire_at  TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (a_number, b_number, x_number)
);

CREATE INDEX idx_axb_x ON axb_bindings(x_number, active);
CREATE INDEX idx_axb_expire ON axb_bindings(expire_at);
