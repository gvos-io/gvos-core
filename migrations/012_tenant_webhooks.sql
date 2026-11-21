-- 012: tenant binding for webhook subscriptions (Phase C tenant
-- plane) + delivery-log tenant column. Additive; existing rows keep
-- the empty default = admin-created, visible to admins only.
ALTER TABLE webhook_subscriptions ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_whsub_tenant ON webhook_subscriptions(tenant_id);
