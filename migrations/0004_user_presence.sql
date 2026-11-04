-- 0004_user_presence.sql: Статусы присутствия сотрудников (в сети, занят, в отпуске, отошел, не беспокоить)
CREATE TABLE IF NOT EXISTS user_presence (
    user_id VARCHAR(64) PRIMARY KEY,
    status VARCHAR(32) NOT NULL DEFAULT 'online', -- online, busy, vacation, away, dnd
    status_text VARCHAR(255) NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_presence_status ON user_presence(status);
