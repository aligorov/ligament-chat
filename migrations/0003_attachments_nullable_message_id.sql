-- 0003_attachments_nullable_message_id.sql
-- Разрешение загрузки вложений до создания сообщения (staged upload)

ALTER TABLE attachments ALTER COLUMN message_id DROP NOT NULL;
