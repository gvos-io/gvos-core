-- v0.4: enforce rate upsert conflict keys. The UNIQUE constraint makes
-- ON CONFLICT (group_id, prefix, priority, effective_at) actually
-- fire - without it every upsert INSERTed a new row and the loader
-- picked stale prices (live acceptance finding).

-- Deduplicate first (keep the newest row per conflict key), then add
-- the constraint.
DELETE FROM rates r
USING rates newer
WHERE r.group_id = newer.group_id
  AND r.prefix = newer.prefix
  AND r.priority = newer.priority
  AND r.effective_at = newer.effective_at
  AND r.ctid < newer.ctid;

ALTER TABLE rates ADD CONSTRAINT rates_conflict_key UNIQUE (group_id, prefix, priority, effective_at);
