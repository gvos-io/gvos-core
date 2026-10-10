-- v0.6: CDR partition maintenance. Run monthly (cron / manual) to
-- pre-create the next month's partition and optionally detach +
-- archive partitions past the retention window.

-- 1. Pre-create the next 3 months (adjust dates at run time).
--    Example for running in December 2026:
-- CREATE TABLE IF NOT EXISTS cdr_2027_01 PARTITION OF cdr
--     FOR VALUES FROM ('2027-01-01') TO ('2027-02-01');
-- CREATE TABLE IF NOT EXISTS cdr_2027_02 PARTITION OF cdr
--     FOR VALUES FROM ('2027-02-01') TO ('2027-03-01');
-- CREATE TABLE IF NOT EXISTS cdr_2027_03 PARTITION OF cdr
--     FOR VALUES FROM ('2027-03-01') TO ('2027-04-01');

-- 2. Retention: detach partitions older than the window (default 90
--    days per the compliance package), then archive the detached
--    table to object storage before dropping:
-- ALTER TABLE cdr DETACH PARTITION cdr_2026_10;
-- -- archive cdr_2026_10 (pg_dump -t or COPY out), then:
-- DROP TABLE cdr_2026_10;

-- NOTE: run via cron with dynamic dates, e.g. a small shell/python
-- job generating the CREATE TABLE ... FOR VALUES statements for
-- now()+3 months. Rollup table usage_daily keeps the billing history
-- regardless of CDR retention, so reports survive archival.
