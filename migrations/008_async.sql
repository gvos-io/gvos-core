-- Async-write hardening: CDR persistence moves to a JetStream
-- consumer (at-least-once delivery), so the write must be idempotent
-- against redeliveries. The unique index collapses duplicates (the
-- partition key start_time must be part of any unique index on a
-- partitioned table; a redelivery carries the same start_time).
-- NOTE the DROP: when this migration runs AFTER partitions already
-- exist and were filled (the 001-initdb order on a fresh volume puts
-- 001 before 008), PostgreSQL creates the parent index as ON ONLY
-- without cascading to partitions — it then constrains NOTHING
-- (measured live: same call_id inserted twice under load). Dropping
-- and re-creating forces PG to attach local unique indexes to every
-- existing partition; future partitions inherit them automatically.
DROP INDEX IF EXISTS uq_cdr_call;
CREATE UNIQUE INDEX uq_cdr_call
    ON cdr (call_id, tenant_id, start_time);
