-- Settings: a small JSONB key/value store for global runtime toggles
-- (recording on/off, risk rule set, package definitions). Not Kamailio
-- data -- gv-core only. Broadcasts (gvos.cfg.reload) carry the change
-- to peer replicas instantly; this table is what restarts and new
-- replicas load back, so a toggle survives process lifecycle.

CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
