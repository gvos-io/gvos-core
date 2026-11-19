-- 011: outbound webhook subscriptions + delivery log (Phase D).
-- Subscriptions bind an HTTPS endpoint to a set of event subjects;
-- deliveries are at-least-once with exponential backoff and a dead
-- letter row for operator-visible failures.

CREATE TABLE webhook_subscriptions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    url        TEXT NOT NULL,
    secret     TEXT NOT NULL,             -- HMAC key, shown once at creation
    events     TEXT[] NOT NULL,           -- subjects this endpoint wants
    active     BOOL NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
    id          BIGSERIAL PRIMARY KEY,
    sub_id      UUID,
    url         TEXT NOT NULL,
    subject     TEXT NOT NULL,
    payload     TEXT NOT NULL,
    status      TEXT NOT NULL,            -- delivered | dead
    attempts    INT NOT NULL DEFAULT 0,
    last_error  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_whdel_status ON webhook_deliveries(status, created_at DESC);
