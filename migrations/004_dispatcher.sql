-- v0.3: dispatcher-compatible columns on gateways.
-- Kamailio dispatcher reads (setid, destination, flags, priority,
-- attrs, description); we map our carrier table onto that shape so
-- routing config and billing data share one table.

ALTER TABLE gateways ADD COLUMN IF NOT EXISTS setid INT NOT NULL DEFAULT 1;
ALTER TABLE gateways ADD COLUMN IF NOT EXISTS destination TEXT;
ALTER TABLE gateways ADD COLUMN IF NOT EXISTS dspriority INT NOT NULL DEFAULT 0;
ALTER TABLE gateways ADD COLUMN IF NOT EXISTS state INT NOT NULL DEFAULT 0;
-- Keep sip_addr as the billing-visible address; destination mirrors
-- it in dispatcher form (sip:host:port).
UPDATE gateways SET destination = 'sip:' || sip_addr WHERE destination IS NULL;

-- Demo gateway seeded into set 1.
INSERT INTO gateways (tenant_id, name, sip_addr, setid, destination, dspriority, max_concurrent, max_cps)
VALUES ('00000000-0000-0000-0000-000000000001', 'demo-gw', 'demo-gw:5060', 1, 'sip:demo-gw:5060', 5, 1000, 100)
ON CONFLICT (tenant_id, name) DO UPDATE SET setid = 1, destination = 'sip:demo-gw:5060';

-- A second (backup) gateway for failover demos: points at the same
-- demo-gw container under a different alias priority.
INSERT INTO gateways (tenant_id, name, sip_addr, setid, destination, dspriority, max_concurrent, max_cps)
VALUES ('00000000-0000-0000-0000-000000000001', 'demo-gw-backup', 'demo-gw:5060', 1, 'sip:demo-gw:5060', 10, 1000, 100)
ON CONFLICT (tenant_id, name) DO UPDATE SET setid = 1, destination = 'sip:demo-gw:5060';

-- Dispatcher on this Kamailio build reads its own table shape when
-- table_name points elsewhere; both shapes are kept compatible.
ALTER TABLE gateways ADD COLUMN IF NOT EXISTS flags INT NOT NULL DEFAULT 0;
ALTER TABLE gateways ADD COLUMN IF NOT EXISTS attrs TEXT DEFAULT '';
ALTER TABLE gateways ADD COLUMN IF NOT EXISTS description TEXT DEFAULT '';

-- Canonical dispatcher table (this build ignores table_name for the
-- version check and the flag columns).
CREATE TABLE IF NOT EXISTS dispatcher (
    id SERIAL PRIMARY KEY,
    setid INT NOT NULL DEFAULT 1,
    destination TEXT NOT NULL,
    flags INT NOT NULL DEFAULT 0,
    priority INT NOT NULL DEFAULT 0,
    attrs TEXT DEFAULT '',
    description TEXT DEFAULT ''
);
INSERT INTO version (table_name, table_version) VALUES ('dispatcher', 4)
ON CONFLICT (table_name) DO UPDATE SET table_version = 4;
ALTER TABLE gateways ADD COLUMN IF NOT EXISTS priority INT NOT NULL DEFAULT 0;
