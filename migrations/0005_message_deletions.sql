-- 0005_message_deletions.sql: Сохранение локальных удалений сообщений ("Удалить у себя")
CREATE TABLE IF NOT EXISTS message_deletions (
    user_id VARCHAR(64) NOT NULL,
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    chat_id UUID NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, message_id)
);

CREATE INDEX IF NOT EXISTS idx_message_deletions_user_chat ON message_deletions(user_id, chat_id);
