-- 016: tenant names are unique among live tenants. The table existed
-- without this since 001 (names were operator-facing display text);
-- with the admin API provisioning tenants, a duplicate name is a
-- mistake worth refusing at the door.
CREATE UNIQUE INDEX IF NOT EXISTS uq_tenants_live_name ON tenants(name) WHERE deleted_at IS NULL;
