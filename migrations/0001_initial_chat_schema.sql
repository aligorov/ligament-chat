-- 0001_initial_chat_schema.sql: Корпоративный мессенджер Ligament Chat

-- 1. Доверенные ключи Core (Ed25519 SSO)
CREATE TABLE IF NOT EXISTS auth_trust (
    instance_id VARCHAR(64) PRIMARY KEY,
    public_key_hex VARCHAR(64) NOT NULL,
    tenant_scope VARCHAR(64) NOT NULL DEFAULT 'default',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 2. Системные настройки чата (Zero-ENV)
CREATE TABLE IF NOT EXISTS chat_settings (
    key VARCHAR(64) PRIMARY KEY,
    value JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 3. Чаты и каналы
CREATE TABLE IF NOT EXISTS chats (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind VARCHAR(32) NOT NULL DEFAULT 'group', -- direct, group, channel, saved
    title VARCHAR(255) NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    avatar_url TEXT NOT NULL DEFAULT '',
    is_forum BOOLEAN NOT NULL DEFAULT FALSE,   -- Режим форума с топиками (Telegram-модель)
    is_archived BOOLEAN NOT NULL DEFAULT FALSE,
    created_by VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_chats_kind ON chats(kind);

-- 4. Топики форума (темы внутри группы)
CREATE TABLE IF NOT EXISTS chat_topics (
    id BIGSERIAL PRIMARY KEY,
    chat_id UUID NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    title VARCHAR(255) NOT NULL,
    icon_emoji VARCHAR(32) NOT NULL DEFAULT '💬',
    is_general BOOLEAN NOT NULL DEFAULT FALSE,
    is_closed BOOLEAN NOT NULL DEFAULT FALSE,
    is_pinned BOOLEAN NOT NULL DEFAULT FALSE,
    created_by VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_chat_topics_title UNIQUE (chat_id, title)
);

CREATE INDEX IF NOT EXISTS idx_chat_topics_chat_id ON chat_topics(chat_id);

-- 5. Участники чатов
CREATE TABLE IF NOT EXISTS chat_members (
    chat_id UUID NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    user_id VARCHAR(64) NOT NULL,
    role VARCHAR(32) NOT NULL DEFAULT 'member', -- admin, moderator, member
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chat_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_chat_members_user_id ON chat_members(user_id);

-- 6. Правила доступа по группам AD / LDAP / Отделам
CREATE TABLE IF NOT EXISTS group_access_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id UUID NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    rule_type VARCHAR(32) NOT NULL, -- ad_group, department, role, user
    target_name VARCHAR(255) NOT NULL,
    can_write BOOLEAN NOT NULL DEFAULT TRUE,
    can_call BOOLEAN NOT NULL DEFAULT TRUE,
    is_moderator BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_group_access_rules_chat ON group_access_rules(chat_id);
CREATE INDEX IF NOT EXISTS idx_group_access_rules_target ON group_access_rules(target_name);

-- 7. Сообщения (строго монотонный ID для надёжного курсора)
CREATE TABLE IF NOT EXISTS messages (
    id BIGSERIAL PRIMARY KEY,
    chat_id UUID NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    topic_id BIGINT NOT NULL DEFAULT 0,
    sender_id VARCHAR(64) NOT NULL,
    sender_name VARCHAR(255) NOT NULL DEFAULT '',
    client_msg_id VARCHAR(64) NOT NULL DEFAULT '',
    reply_to_id BIGINT REFERENCES messages(id) ON DELETE SET NULL,
    body TEXT NOT NULL DEFAULT '',
    kind VARCHAR(32) NOT NULL DEFAULT 'text', -- text, secret, action_card, link_call, system
    secret_ttl_seconds INT DEFAULT NULL,     -- Таймер самоуничтожения (One-Time Secret)
    secret_read_at TIMESTAMPTZ DEFAULT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    edited_at TIMESTAMPTZ DEFAULT NULL,
    deleted_at TIMESTAMPTZ DEFAULT NULL
);

-- Составной индекс для высокоскоростного подсчёта непрочитанных и пагинации (0.59 мс Index Only Scan)
CREATE INDEX IF NOT EXISTS idx_messages_chat_topic_id ON messages (chat_id, topic_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_idempotency ON messages (chat_id, client_msg_id) WHERE client_msg_id <> '';

-- 8. High-Water Mark указатель прочтения (Telegram-модель)
CREATE TABLE IF NOT EXISTS topic_participants (
    chat_id UUID NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    topic_id BIGINT NOT NULL DEFAULT 0,
    user_id VARCHAR(64) NOT NULL,
    last_read_message_id BIGINT NOT NULL DEFAULT 0,
    last_read_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chat_id, topic_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_topic_participants_user ON topic_participants(user_id);

-- 9. Вложения и файлы
CREATE TABLE IF NOT EXISTS attachments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    chat_id UUID NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    file_name VARCHAR(255) NOT NULL,
    file_size BIGINT NOT NULL,
    content_type VARCHAR(128) NOT NULL,
    storage_path TEXT NOT NULL,
    sha256_hex VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_attachments_chat ON attachments(chat_id);
CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments(message_id);

-- 10. WebRTC звонки
CREATE TABLE IF NOT EXISTS call_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id UUID NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    caller_user_id VARCHAR(64) NOT NULL,
    callee_user_id VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'ringing', -- ringing, active, ended, rejected, missed
    is_recorded BOOLEAN NOT NULL DEFAULT FALSE,
    recorder_session_id UUID DEFAULT NULL,
    started_at TIMESTAMPTZ DEFAULT NULL,
    ended_at TIMESTAMPTZ DEFAULT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_call_sessions_chat ON call_sessions(chat_id);
