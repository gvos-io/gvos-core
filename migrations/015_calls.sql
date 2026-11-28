-- 015: origination calls resource (click-to-call / AI outbound, M11 F1)
-- POST /v1/calls creates a row in state 'pending', publishes
-- gvos.call.originate (the outboard UAC dialer issues the INVITE with
-- call_id = calls.id), and the existing signaling event flow
-- (call start/answer/end) drives dialing → answered → ended. The
-- billing lock still rides the normal /v1/call/start path — nothing
-- here bills directly.
CREATE TABLE calls (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    account_id  UUID NOT NULL,
    caller      TEXT NOT NULL,              -- the side the platform dials first (billed side)
    callee      TEXT,                       -- second leg (empty for AI outbound)
    to_agent    TEXT,                       -- F2: media agent destination
    state       TEXT NOT NULL DEFAULT 'pending'
                CHECK (state IN ('pending','dialing','answered','ended','cancelled','failed')),
    fail_reason TEXT,                       -- release reason on ended/failed
    max_duration_s INT NOT NULL DEFAULT 1800,
    custom      JSONB,                      -- caller passthrough metadata
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_calls_account ON calls(account_id, created_at DESC);
CREATE INDEX idx_calls_live ON calls(state) WHERE state IN ('pending','dialing');
