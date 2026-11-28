-- 017: account enable/disable (gap analysis M6 remainder). PATCH
-- /v1/admin/accounts/{id} flips enabled=false: admission (call start
-- + origination) rejects disabled accounts with 403 DISABLED — the
-- row, balance and ledger stay untouched (re-enable is instant).
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS enabled BOOLEAN NOT NULL DEFAULT true;
CREATE INDEX IF NOT EXISTS idx_accounts_disabled ON accounts(enabled) WHERE enabled = false;
