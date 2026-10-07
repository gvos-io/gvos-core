-- G-VOS schema v0.1
-- Mirrors the in-memory model in internal/{account,rate,callstate}.
-- Conventions: money as NUMERIC(18,7) (VOS 7-decimal precision),
-- all timestamps UTC, soft-delete via deleted_at on mutable business
-- rows, append-only audit on cdr/audit_log.

-- gen_random_uuid(): built into PG14+. ltree needs the extension.
CREATE EXTENSION IF NOT EXISTS ltree;

CREATE TABLE tenants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    currency    CHAR(3) NOT NULL DEFAULT 'CNY',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);

-- Account tree: tenant root -> agents (any depth) -> end accounts.
-- parent_id + path makes agent-chain walks index-friendly.
CREATE TABLE accounts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    parent_id   UUID REFERENCES accounts(id),
    name        TEXT NOT NULL,
    username    TEXT NOT NULL,             -- SIP auth username
    secret_hash TEXT NOT NULL,             -- argon2id
    rate_group  TEXT NOT NULL,
    postpaid    BOOLEAN NOT NULL DEFAULT false,
    credit      NUMERIC(18,7) NOT NULL DEFAULT 0 CHECK (credit >= 0 OR postpaid),
    max_debt    NUMERIC(18,7) NOT NULL DEFAULT 0,
    max_concurrent INT NOT NULL DEFAULT 0, -- 0 = unlimited
    max_cps     INT NOT NULL DEFAULT 0,
    path        LTREE,                     -- materialized agent chain
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ,
    UNIQUE (tenant_id, username)
);
CREATE INDEX idx_accounts_parent ON accounts(parent_id);
CREATE INDEX idx_accounts_tenant ON accounts(tenant_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_accounts_path  ON accounts USING GIST (path);

-- Balance movements, append-only. The in-memory Redis balance is the
-- live value; this table is the ledger used for reconciliation and
-- dispute resolution (RPO=0 source of truth).
CREATE TABLE balance_ledger (
    id          BIGSERIAL PRIMARY KEY,
    account_id  UUID NOT NULL REFERENCES accounts(id),
    delta       NUMERIC(18,7) NOT NULL,
    balance     NUMERIC(18,7) NOT NULL,    -- post-movement balance
    reason      TEXT NOT NULL,             -- call-lock|call-settle|refund|topup|adjustment
    call_id     TEXT,
    operator    TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_ledger_account_time ON balance_ledger(account_id, created_at DESC);

-- Rate groups group prefixes; rates carry interval schemes.
CREATE TABLE rate_groups (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL REFERENCES tenants(id),
    name       TEXT NOT NULL,
    UNIQUE (tenant_id, name)
);

CREATE TABLE rates (
    id          BIGSERIAL PRIMARY KEY,
    group_id    UUID NOT NULL REFERENCES rate_groups(id) ON DELETE CASCADE,
    prefix      TEXT NOT NULL,
    price       NUMERIC(18,7) NOT NULL CHECK (price >= 0),  -- per minute
    init_dur    INT NOT NULL DEFAULT 60,
    init_price  NUMERIC(18,7),              -- NULL => price*init_dur/60
    step_dur    INT NOT NULL DEFAULT 6,
    min_dur     INT NOT NULL DEFAULT 0,
    -- time window (weekly); NULL/empty = always in effect
    days        SMALLINT[],                 -- 0=Sun..6=Sat
    win_start   TIME,
    win_end     TIME,
    priority    INT NOT NULL DEFAULT 0,     -- windowed > base regardless; tie-break
    effective_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at    TIMESTAMPTZ,                -- superseded rows keep history
    UNIQUE (group_id, prefix, priority, effective_at)
);
CREATE INDEX idx_rates_lookup ON rates(group_id, prefix);

-- Gateways: where calls terminate. Auto-lock fields mirror the
-- "failed N times -> lock M minutes" behavior.
CREATE TABLE gateways (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    name        TEXT NOT NULL,
    sip_addr    TEXT NOT NULL,              -- host:port
    protocol    TEXT NOT NULL DEFAULT 'udp',-- udp|tcp|tls
    username    TEXT,
    secret_hash TEXT,
    max_concurrent INT NOT NULL DEFAULT 0,
    max_cps     INT NOT NULL DEFAULT 0,
    codec_prefs TEXT[],                     -- prefer list, e.g. {g711a,op}
    weight      INT NOT NULL DEFAULT 100,
    locked_until TIMESTAMPTZ,
    consecutive_failures INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ,
    UNIQUE (tenant_id, name)
);

-- Routing: ordered rules per rate group.
CREATE TABLE routes (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    name        TEXT NOT NULL,
    dest_prefix TEXT,                       -- NULL = any
    strategy    TEXT NOT NULL DEFAULT 'priority', -- priority|lcr|ratio|asr|time
    priority    INT NOT NULL DEFAULT 100,
    failover    BOOLEAN NOT NULL DEFAULT true,   -- serial retry on failure
    guard_route BOOLEAN NOT NULL DEFAULT false,  -- fallback after N failures
    active      BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE route_legs (
    route_id    BIGSERIAL REFERENCES routes(id) ON DELETE CASCADE,
    gateway_id  UUID NOT NULL REFERENCES gateways(id),
    ratio       INT NOT NULL DEFAULT 100,
    PRIMARY KEY (route_id, gateway_id)
);

-- CDRs: the billing record. Partitioned by month for retention and
-- query speed (30-day hot window per PRD).
CREATE TABLE cdr (
    id          BIGSERIAL,
    call_id     TEXT NOT NULL,
    tenant_id   UUID NOT NULL,
    account_id  UUID NOT NULL,
    agent_path  TEXT,                       -- chain snapshot at call time
    dest        TEXT NOT NULL,
    dest_prefix TEXT,                       -- matched rate prefix
    gateway_id  UUID,
    start_time  TIMESTAMPTZ NOT NULL,
    answer_time TIMESTAMPTZ,
    end_time    TIMESTAMPTZ,
    duration    INT NOT NULL DEFAULT 0,     -- connected seconds
    billed_secs INT NOT NULL DEFAULT 0,
    price       NUMERIC(18,7) NOT NULL DEFAULT 0, -- per-minute applied
    cost        NUMERIC(18,7) NOT NULL DEFAULT 0,
    currency    CHAR(3) NOT NULL DEFAULT 'CNY',
    sip_code    INT,                        -- final SIP response
    release     TEXT,                       -- normal|balance-exhausted|...
    mos         REAL,                       -- QoS (RTPengine stats)
    jitter_ms   REAL,
    packet_loss REAL,
    pdd_ms      INT,                        -- post-dial delay
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, start_time)
) PARTITION BY RANGE (start_time);

-- Create the first three monthly partitions; a cron/worker adds the
-- next ones ahead of time.
CREATE TABLE cdr_2026_10 PARTITION OF cdr
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE cdr_2026_11 PARTITION OF cdr
    FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
CREATE TABLE cdr_2026_12 PARTITION OF cdr
    FOR VALUES FROM ('2026-12-01') TO ('2027-01-01');

CREATE INDEX idx_cdr_call ON cdr(call_id);
CREATE INDEX idx_cdr_account_time ON cdr(account_id, start_time DESC);
CREATE INDEX idx_cdr_tenant_time ON cdr(tenant_id, start_time DESC);
CREATE INDEX idx_cdr_gateway ON cdr(gateway_id) WHERE gateway_id IS NOT NULL;

-- Black/white lists (global + per-tenant).
CREATE TABLE number_lists (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   UUID REFERENCES tenants(id), -- NULL = global
    kind        TEXT NOT NULL CHECK (kind IN ('black', 'white')),
    scope       TEXT NOT NULL DEFAULT 'dest' CHECK (scope IN ('src', 'dest')),
    pattern     TEXT NOT NULL,               -- prefix or regex
    is_regex    BOOLEAN NOT NULL DEFAULT false,
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_lists_tenant ON number_lists(tenant_id, kind, scope);

-- Append-only audit: every mutation of billing-relevant config.
CREATE TABLE audit_log (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   UUID,
    operator    TEXT NOT NULL,
    ip          INET,
    action      TEXT NOT NULL,               -- create|update|delete|login|...
    entity      TEXT NOT NULL,               -- account|rate|route|gateway|...
    entity_id   TEXT,
    before      JSONB,
    after       JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_entity ON audit_log(entity, entity_id, created_at DESC);
CREATE INDEX idx_audit_tenant ON audit_log(tenant_id, created_at DESC);

-- Usage rollups for reporting (T+1 worker fills these).
CREATE TABLE usage_daily (
    day         DATE NOT NULL,
    tenant_id   UUID NOT NULL,
    account_id  UUID NOT NULL,
    calls       INT NOT NULL DEFAULT 0,
    answered    INT NOT NULL DEFAULT 0,
    duration    INT NOT NULL DEFAULT 0,
    billed_secs INT NOT NULL DEFAULT 0,
    cost        NUMERIC(18,7) NOT NULL DEFAULT 0,
    PRIMARY KEY (day, tenant_id, account_id)
);
