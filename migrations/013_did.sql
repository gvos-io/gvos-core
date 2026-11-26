-- 013: DID inbound termination registry (callee-billed).
-- A carrier INVITE for a DID number resolves here: the account that
-- OWNS the number is the billing account (被叫方计费, pipecat O1),
-- rated by that account's own rate_group (inbound tariff = its own
-- group; no schema change to rates). destination_type reserves the
-- media_agent seam (AI voice-agent; answered 501 until implemented).

CREATE TABLE dids (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    did_number       TEXT NOT NULL UNIQUE,     -- E.164 DID owned on the trunk
    account_id       UUID NOT NULL,            -- callee-side billing account
    destination_type TEXT NOT NULL DEFAULT 'user'
                     CHECK (destination_type IN ('user','external','media_agent')),
    destination      TEXT NOT NULL,            -- SIP username, E.164, or agent id
    enabled          BOOL NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_did_enabled ON dids(enabled) WHERE enabled;

-- CDR direction + gateway attribution (column exists since 001 but
-- was never populated; direction was nowhere in the model).
ALTER TABLE cdr ADD COLUMN IF NOT EXISTS direction TEXT NOT NULL DEFAULT 'out'
    CHECK (direction IN ('out','in'));
