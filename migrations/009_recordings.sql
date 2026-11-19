-- 009: recording index (recording-oss-upgrade-plan §3). One row per
-- (call, track): the uploader's output bookkeeping — where the
-- transcoded artifact landed, which leg it belongs to, and its
-- lifecycle state. The CDR's recorded flag stays the per-call boolean;
-- this table is the per-track detail behind the play button.

CREATE TABLE recordings (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    call_id     TEXT NOT NULL,
    track       TEXT NOT NULL CHECK (track IN ('agent', 'user')),
    object_key  TEXT NOT NULL,            -- storage backend key (oss path / local path)
    locator     TEXT NOT NULL DEFAULT '', -- backend-resolved locator (file path / oss URL)
    format      TEXT NOT NULL DEFAULT 'mp3',
    duration_s  INT,
    size_bytes  BIGINT,
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','ready','failed')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (call_id, track, format)
);

CREATE INDEX idx_recordings_call ON recordings(call_id, status);
