-- Async-write hardening: CDR persistence moves to a JetStream
-- consumer (at-least-once delivery), so the write must be idempotent
-- against redeliveries. The unique index collapses duplicates (the
-- partition key start_time must be part of any unique index on a
-- partitioned table; a redelivery carries the same start_time).
CREATE UNIQUE INDEX IF NOT EXISTS uq_cdr_call
    ON cdr (call_id, tenant_id, start_time);
