-- v0.2: recorded flag on CDRs (rtpengine recording was engaged; the
-- pcap/metadata files are named by call-id in the recording volume).

ALTER TABLE cdr ADD COLUMN IF NOT EXISTS recorded BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS idx_cdr_recorded ON cdr(recorded) WHERE recorded;
