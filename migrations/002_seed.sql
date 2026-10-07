-- Dev/demo seed data for the spike (idempotent-ish: run on a fresh DB).

INSERT INTO tenants (id, name) VALUES
  ('00000000-0000-0000-0000-000000000001', 'Demo Tenant')
ON CONFLICT DO NOTHING;

INSERT INTO rate_groups (id, tenant_id, name) VALUES
  ('00000000-0000-0000-0000-000000000010', '00000000-0000-0000-0000-000000000001', 'demo')
ON CONFLICT DO NOTHING;

-- Agent chain: tenant root (postpaid) -> agent1 (prepaid) -> user 1000.
INSERT INTO accounts (id, tenant_id, parent_id, name, username, secret_hash, rate_group, postpaid, credit) VALUES
  ('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-000000000001', NULL,        'Demo Tenant',  'tenant',  'x', 'demo', true,  0),
  ('00000000-0000-0000-0000-0000000000a2', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000a1', 'Agent One', 'agent1', 'x', 'demo', false, 1000),
  ('00000000-0000-0000-0000-0000000000a3', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000a2', 'Demo SIP user', '1000', 'x', 'demo', false, 10)
ON CONFLICT DO NOTHING;

-- Mirrors the in-memory demo rate table.
INSERT INTO rates (group_id, prefix, price, init_dur, step_dur) VALUES
  ('00000000-0000-0000-0000-000000000010', '86',   0.06, 60, 6),
  ('00000000-0000-0000-0000-000000000010', '8613', 0.05, 60, 6),
  ('00000000-0000-0000-0000-000000000010', '1',    0.03, 60, 6),
  ('00000000-0000-0000-0000-000000000010', '91',   0.90, 60, 6)
ON CONFLICT DO NOTHING;

-- Night discount on US numbers: weekdays 20:00-08:00 half price.
INSERT INTO rates (group_id, prefix, price, init_dur, step_dur, days, win_start, win_end) VALUES
  ('00000000-0000-0000-0000-000000000010', '1', 0.015, 60, 6, '{1,2,3,4,5}', '20:00', '08:00')
ON CONFLICT DO NOTHING;

INSERT INTO gateways (id, tenant_id, name, sip_addr, max_concurrent, max_cps) VALUES
  ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-000000000001', 'demo-gw', 'demo-gw:5060', 1000, 100)
ON CONFLICT DO NOTHING;

INSERT INTO routes (tenant_id, name, dest_prefix, strategy, priority) VALUES
  ('00000000-0000-0000-0000-000000000001', 'default', NULL, 'priority', 100)
ON CONFLICT DO NOTHING;
