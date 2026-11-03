-- 0002_chat_members_display_name.sql
-- Добавление имени участника (ФИО / Display Name) в chat_members для красивого отображения собеседников

ALTER TABLE chat_members ADD COLUMN IF NOT EXISTS display_name VARCHAR(255) NOT NULL DEFAULT '';
