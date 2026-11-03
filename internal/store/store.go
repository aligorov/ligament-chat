package store

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/aligorov/ligament-chat/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore() *Store {
	return &Store{}
}

func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

func (s *Store) AttachPostgres(ctx context.Context, connStr string) error {
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return fmt.Errorf("store: parse pgx config: %w", err)
	}
	cfg.MaxConns = 25
	cfg.MinConns = 2
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("store: connect to postgres: %w", err)
	}
	s.pool = pool

	return s.migrate(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Консультативная блокировка миграций chat: 7710097
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7710097)`); err != nil {
		return fmt.Errorf("store: advisory lock failed: %w", err)
	}

	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS chat_schema_migrations (
		name TEXT PRIMARY KEY,
		checksum TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("store: create migrations table: %w", err)
	}

	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	for _, name := range names {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])

		var prevChecksum string
		err = tx.QueryRow(ctx, `SELECT checksum FROM chat_schema_migrations WHERE name = $1`, name).Scan(&prevChecksum)
		if err == nil {
			if prevChecksum != checksum {
				return fmt.Errorf("store: migration %s checksum mismatch", name)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("store: run migration %s: %w", name, err)
		}

		if _, err := tx.Exec(ctx, `INSERT INTO chat_schema_migrations (name, checksum) VALUES ($1, $2)`, name, checksum); err != nil {
			return fmt.Errorf("store: record migration %s: %w", name, err)
		}
	}

	return tx.Commit(ctx)
}

// SaveTrust регистрирует или обновляет публичный ключ Core.
func (s *Store) SaveTrust(ctx context.Context, instanceID, pubKeyHex, tenantScope string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO auth_trust (instance_id, public_key_hex, tenant_scope, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (instance_id) DO UPDATE
		SET public_key_hex = EXCLUDED.public_key_hex,
		    tenant_scope = EXCLUDED.tenant_scope,
		    updated_at = NOW()
	`, instanceID, pubKeyHex, tenantScope)
	return err
}

// GetTrust возвращает Ed25519 публичный ключ для указанного instance_id.
func (s *Store) GetTrust(ctx context.Context, instanceID string) (ed25519.PublicKey, string, error) {
	var pubHex, tenant string
	err := s.pool.QueryRow(ctx, `SELECT public_key_hex, tenant_scope FROM auth_trust WHERE instance_id = $1`, instanceID).Scan(&pubHex, &tenant)
	if err != nil {
		return nil, "", err
	}
	pubBytes, err := hex.DecodeString(pubHex)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return nil, "", errors.New("store: invalid public key bytes in db")
	}
	return ed25519.PublicKey(pubBytes), tenant, nil
}

// CreateChat создаёт новый чат с топиком по умолчанию (если форум).
func (s *Store) CreateChat(ctx context.Context, chat *Chat, initialMembers []string, rules []GroupAccessRule) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if chat.ID == uuid.Nil {
		chat.ID = uuid.New()
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO chats (id, kind, title, description, avatar_url, is_forum, is_archived, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
		RETURNING created_at, updated_at
	`, chat.ID, chat.Kind, chat.Title, chat.Description, chat.AvatarURL, chat.IsForum, chat.IsArchived, chat.CreatedBy).
		Scan(&chat.CreatedAt, &chat.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create chat: %w", err)
	}

	// Если чат в режиме форума, создаём основной топик "Общий"
	if chat.IsForum {
		_, err = tx.Exec(ctx, `
			INSERT INTO chat_topics (chat_id, title, icon_emoji, is_general, created_by, created_at, updated_at)
			VALUES ($1, 'Общий', '💬', TRUE, $2, NOW(), NOW())
			ON CONFLICT (chat_id, title) DO NOTHING
		`, chat.ID, chat.CreatedBy)
		if err != nil {
			return fmt.Errorf("create general topic: %w", err)
		}
	}

	// Добавляем создателя как admin
	_, err = tx.Exec(ctx, `
		INSERT INTO chat_members (chat_id, user_id, role, joined_at)
		VALUES ($1, $2, 'admin', NOW())
		ON CONFLICT (chat_id, user_id) DO NOTHING
	`, chat.ID, chat.CreatedBy)
	if err != nil {
		return fmt.Errorf("add creator member: %w", err)
	}

	// Добавляем остальных участников
	for _, m := range initialMembers {
		if m == chat.CreatedBy || m == "" {
			continue
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO chat_members (chat_id, user_id, role, joined_at)
			VALUES ($1, $2, 'member', NOW())
			ON CONFLICT (chat_id, user_id) DO NOTHING
		`, chat.ID, m)
		if err != nil {
			return fmt.Errorf("add member %s: %w", m, err)
		}
	}

	// Добавляем правила доступа
	for _, r := range rules {
		_, err = tx.Exec(ctx, `
			INSERT INTO group_access_rules (id, chat_id, rule_type, target_name, can_write, can_call, is_moderator, created_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, NOW())
		`, chat.ID, r.RuleType, r.TargetName, r.CanWrite, r.CanCall, r.IsModerator)
		if err != nil {
			return fmt.Errorf("add access rule: %w", err)
		}
	}

	return tx.Commit(ctx)
}

// GetChat возвращает информацию о чате по ID (с адаптивным заголовком для direct-диалогов).
func (s *Store) GetChat(ctx context.Context, chatID uuid.UUID, forUserID string) (*Chat, error) {
	var c Chat
	err := s.pool.QueryRow(ctx, `
		SELECT c.id, c.kind,
		       CASE
		           WHEN c.kind = 'direct' THEN COALESCE(
		               NULLIF((SELECT cm_peer.display_name FROM chat_members cm_peer WHERE cm_peer.chat_id = c.id AND cm_peer.user_id != $2 LIMIT 1), ''),
		               NULLIF((SELECT cm_peer.user_id FROM chat_members cm_peer WHERE cm_peer.chat_id = c.id AND cm_peer.user_id != $2 LIMIT 1), ''),
		               c.title
		           )
		           ELSE c.title
		       END AS title,
		       c.description, c.avatar_url, c.is_forum, c.is_archived, c.created_by, c.created_at, c.updated_at
		FROM chats c WHERE c.id = $1
	`, chatID, forUserID).Scan(&c.ID, &c.Kind, &c.Title, &c.Description, &c.AvatarURL, &c.IsForum, &c.IsArchived, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetOrCreateDirectChat находит существующий или создаёт новый прямой диалог 1-на-1 между user1 и user2.
func (s *Store) GetOrCreateDirectChat(ctx context.Context, user1, user2, user1Name, user2Name string) (*Chat, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("store: nil pool")
	}

	user1 = strings.TrimSpace(user1)
	user2 = strings.TrimSpace(user2)
	if user1 == "" || user2 == "" {
		return nil, fmt.Errorf("store: empty users for direct chat")
	}
	user1Name = strings.TrimSpace(user1Name)
	if user1Name == "" {
		user1Name = user1
	}
	user2Name = strings.TrimSpace(user2Name)
	if user2Name == "" {
		user2Name = user2
	}

	// 1. Поиск существующего direct-диалога между этими двумя участниками
	var existID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT cm1.chat_id
		FROM chat_members cm1
		JOIN chat_members cm2 ON cm1.chat_id = cm2.chat_id
		JOIN chats c ON c.id = cm1.chat_id
		WHERE c.kind = 'direct' AND cm1.user_id = $1 AND cm2.user_id = $2
		LIMIT 1
	`, user1, user2).Scan(&existID)

	if err == nil && existID != uuid.Nil {
		// Освежаем display_name участников при наличии актуальных имен
		if user1Name != "" && user1Name != user1 {
			_, _ = s.pool.Exec(ctx, `
				UPDATE chat_members SET display_name = $3 WHERE chat_id = $1 AND user_id = $2
			`, existID, user1, user1Name)
		}
		if user2Name != "" && user2Name != user2 {
			_, _ = s.pool.Exec(ctx, `
				UPDATE chat_members SET display_name = $3 WHERE chat_id = $1 AND user_id = $2
			`, existID, user2, user2Name)
		}
		return s.GetChat(ctx, existID, user1)
	}

	// 2. Создание нового диалога
	chatID := uuid.New()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin direct chat tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	chat := &Chat{
		ID:        chatID,
		Kind:      ChatKindDirect,
		Title:     user2Name,
		CreatedBy: user1,
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO chats (id, kind, title, description, avatar_url, is_forum, is_archived, created_by, created_at, updated_at)
		VALUES ($1, 'direct', $2, '', '', FALSE, FALSE, $3, NOW(), NOW())
		RETURNING created_at, updated_at
	`, chatID, user2Name, user1).Scan(&chat.CreatedAt, &chat.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert direct chat: %w", err)
	}

	// Добавляем обоих участников с их читаемыми именами (display_name)
	_, err = tx.Exec(ctx, `
		INSERT INTO chat_members (chat_id, user_id, display_name, role, joined_at)
		VALUES ($1, $2, $3, 'member', NOW())
		ON CONFLICT (chat_id, user_id) DO UPDATE SET display_name = EXCLUDED.display_name
	`, chatID, user1, user1Name)
	if err != nil {
		return nil, fmt.Errorf("add direct member 1: %w", err)
	}

	if user1 != user2 {
		_, err = tx.Exec(ctx, `
			INSERT INTO chat_members (chat_id, user_id, display_name, role, joined_at)
			VALUES ($1, $2, $3, 'member', NOW())
			ON CONFLICT (chat_id, user_id) DO UPDATE SET display_name = EXCLUDED.display_name
		`, chatID, user2, user2Name)
		if err != nil {
			return nil, fmt.Errorf("add direct member 2: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit direct chat: %w", err)
	}

	return s.GetChat(ctx, chatID, user1)
}

// DeleteChat удаляет чат и все связанные каскадные сущности (сообщения, топики, вложения).
func (s *Store) DeleteChat(ctx context.Context, chatID uuid.UUID, userID string, isAdmin bool) error {
	var kind, createdBy string
	err := s.pool.QueryRow(ctx, `SELECT kind, created_by FROM chats WHERE id = $1`, chatID).Scan(&kind, &createdBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}

	if !isAdmin {
		if kind == "direct" {
			var isMember bool
			_ = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chat_members WHERE chat_id = $1 AND user_id = $2)`, chatID, userID).Scan(&isMember)
			if !isMember {
				return fmt.Errorf("forbidden: not a participant of this direct chat")
			}
		} else if createdBy != userID {
			return fmt.Errorf("forbidden: only creator or admin can delete chat")
		}
	}

	_, err = s.pool.Exec(ctx, `DELETE FROM chats WHERE id = $1`, chatID)
	return err
}

// ListUserChats возвращает список чатов пользователя с вычисленным количеством непрочитанных сообщений.
func (s *Store) ListUserChats(ctx context.Context, userID string, groups []string, dept, role string) ([]Chat, error) {
	// Суперпользователь или chat_admin видит все чаты, иначе только те, где участник или есть правило доступа
	isAdmin := role == "chat_admin" || role == "admin" || role == "superadmin"

	query := `
		SELECT c.id, c.kind,
		       CASE
		           WHEN c.kind = 'direct' THEN COALESCE(
		               NULLIF((SELECT cm_peer.display_name FROM chat_members cm_peer WHERE cm_peer.chat_id = c.id AND cm_peer.user_id != $1 LIMIT 1), ''),
		               NULLIF((SELECT cm_peer.user_id FROM chat_members cm_peer WHERE cm_peer.chat_id = c.id AND cm_peer.user_id != $1 LIMIT 1), ''),
		               c.title
		           )
		           ELSE c.title
		       END AS title,
		       c.description, c.avatar_url, c.is_forum, c.is_archived, c.created_by, c.created_at, c.updated_at,
		       COALESCE((
		           SELECT COUNT(*)
		           FROM messages m
		           LEFT JOIN topic_participants tp ON tp.chat_id = m.chat_id AND tp.topic_id = m.topic_id AND tp.user_id = $1
		           WHERE m.chat_id = c.id AND m.id > COALESCE(tp.last_read_message_id, 0)
		       ), 0) AS unread_count,
		       COALESCE((SELECT cm_peer.user_id FROM chat_members cm_peer WHERE cm_peer.chat_id = c.id AND cm_peer.user_id != $1 LIMIT 1), '') AS peer_user_id
		FROM chats c
		WHERE c.is_archived = FALSE AND (
		    $2 = TRUE
		    OR EXISTS (SELECT 1 FROM chat_members cm WHERE cm.chat_id = c.id AND cm.user_id = $1)
		    OR EXISTS (
		        SELECT 1 FROM group_access_rules gar
		        WHERE gar.chat_id = c.id AND (
		            (gar.rule_type = 'ad_group' AND gar.target_name = ANY($3))
		            OR (gar.rule_type = 'department' AND gar.target_name = $4)
		            OR (gar.rule_type = 'user' AND gar.target_name = $1)
		        )
		    )
		)
		ORDER BY c.updated_at DESC
	`

	rows, err := s.pool.Query(ctx, query, userID, isAdmin, groups, dept)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.Kind, &c.Title, &c.Description, &c.AvatarURL, &c.IsForum, &c.IsArchived, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.UnreadCount, &c.PeerUserID); err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// CreateTopic создаёт новый топик в форуме.
func (s *Store) CreateTopic(ctx context.Context, topic *ChatTopic) error {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO chat_topics (chat_id, title, icon_emoji, is_general, is_closed, is_pinned, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		RETURNING id, created_at, updated_at
	`, topic.ChatID, topic.Title, topic.IconEmoji, topic.IsGeneral, topic.IsClosed, topic.IsPinned, topic.CreatedBy).
		Scan(&topic.ID, &topic.CreatedAt, &topic.UpdatedAt)
	return err
}

// ListChatTopics возвращает топики форума с количеством непрочитанных сообщений для пользователя.
func (s *Store) ListChatTopics(ctx context.Context, chatID uuid.UUID, userID string) ([]ChatTopic, error) {
	query := `
		SELECT ct.id, ct.chat_id, ct.title, ct.icon_emoji, ct.is_general, ct.is_closed, ct.is_pinned, ct.created_by, ct.created_at, ct.updated_at,
		       COALESCE((
		           SELECT COUNT(*)
		           FROM messages m
		           LEFT JOIN topic_participants tp ON tp.chat_id = m.chat_id AND tp.topic_id = m.topic_id AND tp.user_id = $2
		           WHERE m.chat_id = ct.chat_id AND m.topic_id = ct.id AND m.id > COALESCE(tp.last_read_message_id, 0)
		       ), 0) AS unread_count
		FROM chat_topics ct
		WHERE ct.chat_id = $1
		ORDER BY ct.is_pinned DESC, ct.is_general DESC, ct.updated_at DESC
	`
	rows, err := s.pool.Query(ctx, query, chatID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var topics []ChatTopic
	for rows.Next() {
		var t ChatTopic
		if err := rows.Scan(&t.ID, &t.ChatID, &t.Title, &t.IconEmoji, &t.IsGeneral, &t.IsClosed, &t.IsPinned, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt, &t.UnreadCount); err != nil {
			return nil, err
		}
		topics = append(topics, t)
	}
	return topics, rows.Err()
}

// SendMessage сохраняет сообщение в чат с монотонным ID и поддержкой идемпотентности.
// SendMessage сохраняет сообщение в чат с монотонным ID, поддержкой идемпотентности и привязкой вложений.
func (s *Store) SendMessage(ctx context.Context, msg *Message, attachmentIDs []uuid.UUID) error {
	metaJSON, err := json.Marshal(msg.Metadata)
	if err != nil {
		metaJSON = []byte("{}")
	}

	err = s.pool.QueryRow(ctx, `
		INSERT INTO messages (chat_id, topic_id, sender_id, sender_name, client_msg_id, reply_to_id, body, kind, secret_ttl_seconds, secret_read_at, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
		ON CONFLICT (chat_id, client_msg_id) WHERE client_msg_id <> ''
		DO UPDATE SET edited_at = messages.edited_at
		RETURNING id, created_at
	`, msg.ChatID, msg.TopicID, msg.SenderID, msg.SenderName, msg.ClientMsgID, msg.ReplyToID, msg.Body, msg.Kind, msg.SecretTTLSeconds, msg.SecretReadAt, metaJSON).
		Scan(&msg.ID, &msg.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: insert message: %w", err)
	}

	// Привязываем вложения к созданному сообщению
	if len(attachmentIDs) > 0 {
		_, _ = s.pool.Exec(ctx, `
			UPDATE attachments
			SET message_id = $1
			WHERE id = ANY($2) AND chat_id = $3
		`, msg.ID, attachmentIDs, msg.ChatID)

		attRows, aErr := s.pool.Query(ctx, `
			SELECT id, message_id, chat_id, file_name, file_size, content_type, storage_path, sha256_hex, created_at
			FROM attachments WHERE message_id = $1
		`, msg.ID)
		if aErr == nil {
			defer attRows.Close()
			for attRows.Next() {
				var a Attachment
				if scanErr := attRows.Scan(&a.ID, &a.MessageID, &a.ChatID, &a.FileName, &a.FileSize, &a.ContentType, &a.StoragePath, &a.SHA256Hex, &a.CreatedAt); scanErr == nil {
					msg.Attachments = append(msg.Attachments, a)
				}
			}
		}
	}

	// При наличии читаемого имени отправителя обновляем display_name в участниках чата
	if msg.SenderName != "" && msg.SenderName != msg.SenderID {
		_, _ = s.pool.Exec(ctx, `
			UPDATE chat_members
			SET display_name = $3
			WHERE chat_id = $1 AND user_id = $2 AND (display_name = '' OR display_name = user_id)
		`, msg.ChatID, msg.SenderID, msg.SenderName)
	}

	// Обновляем updated_at у чата и топика
	_, _ = s.pool.Exec(ctx, `UPDATE chats SET updated_at = NOW() WHERE id = $1`, msg.ChatID)
	if msg.TopicID > 0 {
		_, _ = s.pool.Exec(ctx, `UPDATE chat_topics SET updated_at = NOW() WHERE id = $1`, msg.TopicID)
	}

	return nil
}

// ListMessages возвращает сообщения чата/топика до указанного beforeID (пагинация назад) или последние, включая вложения.
func (s *Store) ListMessages(ctx context.Context, chatID uuid.UUID, topicID int64, beforeID int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	var rows pgx.Rows
	var err error

	if beforeID > 0 {
		rows, err = s.pool.Query(ctx, `
			SELECT id, chat_id, topic_id, sender_id, sender_name, client_msg_id, reply_to_id, body, kind, secret_ttl_seconds, secret_read_at, metadata, created_at, edited_at, deleted_at
			FROM messages
			WHERE chat_id = $1 AND topic_id = $2 AND id < $3
			ORDER BY id DESC
			LIMIT $4
		`, chatID, topicID, beforeID, limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT id, chat_id, topic_id, sender_id, sender_name, client_msg_id, reply_to_id, body, kind, secret_ttl_seconds, secret_read_at, metadata, created_at, edited_at, deleted_at
			FROM messages
			WHERE chat_id = $1 AND topic_id = $2
			ORDER BY id DESC
			LIMIT $3
		`, chatID, topicID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	var msgIDs []int64
	for rows.Next() {
		var m Message
		var metaRaw []byte
		if err := rows.Scan(&m.ID, &m.ChatID, &m.TopicID, &m.SenderID, &m.SenderName, &m.ClientMsgID, &m.ReplyToID, &m.Body, &m.Kind, &m.SecretTTLSeconds, &m.SecretReadAt, &metaRaw, &m.CreatedAt, &m.EditedAt, &m.DeletedAt); err != nil {
			return nil, err
		}
		if len(metaRaw) > 0 {
			_ = json.Unmarshal(metaRaw, &m.Metadata)
		}
		msgs = append(msgs, m)
		msgIDs = append(msgIDs, m.ID)
	}

	// Загружаем вложения для всех выбранных сообщений
	if len(msgIDs) > 0 {
		attRows, aErr := s.pool.Query(ctx, `
			SELECT id, message_id, chat_id, file_name, file_size, content_type, storage_path, sha256_hex, created_at
			FROM attachments
			WHERE chat_id = $1 AND message_id = ANY($2)
		`, chatID, msgIDs)
		if aErr == nil {
			defer attRows.Close()
			attsByMsg := make(map[int64][]Attachment)
			for attRows.Next() {
				var a Attachment
				if scanErr := attRows.Scan(&a.ID, &a.MessageID, &a.ChatID, &a.FileName, &a.FileSize, &a.ContentType, &a.StoragePath, &a.SHA256Hex, &a.CreatedAt); scanErr == nil {
					attsByMsg[a.MessageID] = append(attsByMsg[a.MessageID], a)
				}
			}
			for i := range msgs {
				if atts, ok := attsByMsg[msgs[i].ID]; ok {
					msgs[i].Attachments = atts
				}
			}
		}
	}

	// Разворачиваем в хронологический порядок (id ASC)
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}

	return msgs, rows.Err()
}

// UpdateReadPointer смещает High-Water Mark указатель прочтения (только вперёд!).
func (s *Store) UpdateReadPointer(ctx context.Context, chatID uuid.UUID, topicID int64, userID string, lastReadID int64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO topic_participants (chat_id, topic_id, user_id, last_read_message_id, last_read_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (chat_id, topic_id, user_id) DO UPDATE
		SET last_read_message_id = GREATEST(topic_participants.last_read_message_id, EXCLUDED.last_read_message_id),
		    last_read_at = NOW()
	`, chatID, topicID, userID, lastReadID)
	return err
}

// CanAccessChat проверяет права пользователя на чтение/запись в чат.
func (s *Store) CanAccessChat(ctx context.Context, chatID uuid.UUID, userID string, groups []string, dept, role string) (bool, string, error) {
	if role == "chat_admin" || role == "admin" || role == "superadmin" {
		return true, MemberRoleAdmin, nil
	}

	// Проверяем прямое членство в чате
	var memberRole string
	err := s.pool.QueryRow(ctx, `SELECT role FROM chat_members WHERE chat_id = $1 AND user_id = $2`, chatID, userID).Scan(&memberRole)
	if err == nil {
		return true, memberRole, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, "", err
	}

	// Проверяем правила доступа (AD-группы, отдел)
	var canWrite, isMod bool
	err = s.pool.QueryRow(ctx, `
		SELECT bool_or(can_write), bool_or(is_moderator)
		FROM group_access_rules
		WHERE chat_id = $1 AND (
			(rule_type = 'ad_group' AND target_name = ANY($2))
			OR (rule_type = 'department' AND target_name = $3)
			OR (rule_type = 'user' AND target_name = $4)
		)
	`, chatID, groups, dept, userID).Scan(&canWrite, &isMod)
	if err == nil {
		effective := MemberRoleMember
		if isMod {
			effective = MemberRoleModerator
		} else if !canWrite {
			effective = "viewer"
		}
		return true, effective, nil
	}

	return false, "", nil
}

// CreateCallSession создаёт сессию WebRTC звонка.
func (s *Store) CreateCallSession(ctx context.Context, call *CallSession) error {
	if call.ID == uuid.Nil {
		call.ID = uuid.New()
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO call_sessions (id, chat_id, caller_user_id, callee_user_id, status, is_recorded, recorder_session_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		RETURNING created_at
	`, call.ID, call.ChatID, call.CallerUserID, call.CalleeUserID, call.Status, call.IsRecorded, call.RecorderSessionID).
		Scan(&call.CreatedAt)
	return err
}

// UpdateCallSession обновляет статус и запись WebRTC звонка.
func (s *Store) UpdateCallSession(ctx context.Context, callID uuid.UUID, status string, isRecorded bool, recID *uuid.UUID) error {
	var startedAt, endedAt *time.Time
	now := time.Now().UTC()
	if status == "active" {
		startedAt = &now
	} else if status == "ended" || status == "rejected" || status == "missed" {
		endedAt = &now
	}

	_, err := s.pool.Exec(ctx, `
		UPDATE call_sessions
		SET status = $2,
		    is_recorded = $3,
		    recorder_session_id = COALESCE($4, recorder_session_id),
		    started_at = COALESCE($5, started_at),
		    ended_at = COALESCE($6, ended_at)
		WHERE id = $1
	`, callID, status, isRecorded, recID, startedAt, endedAt)
	return err
}

// SaveAttachment сохраняет метаданные файла вложения.
func (s *Store) SaveAttachment(ctx context.Context, att *Attachment) error {
	if att.ID == uuid.Nil {
		att.ID = uuid.New()
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO attachments (id, message_id, chat_id, file_name, file_size, content_type, storage_path, sha256_hex, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		RETURNING created_at
	`, att.ID, att.MessageID, att.ChatID, att.FileName, att.FileSize, att.ContentType, att.StoragePath, att.SHA256Hex).
		Scan(&att.CreatedAt)
	return err
}

// GetAttachment возвращает вложение по ID.
func (s *Store) GetAttachment(ctx context.Context, id uuid.UUID) (*Attachment, error) {
	var a Attachment
	err := s.pool.QueryRow(ctx, `
		SELECT id, message_id, chat_id, file_name, file_size, content_type, storage_path, sha256_hex, created_at
		FROM attachments WHERE id = $1
	`, id).Scan(&a.ID, &a.MessageID, &a.ChatID, &a.FileName, &a.FileSize, &a.ContentType, &a.StoragePath, &a.SHA256Hex, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetSetting читает настройку из таблицы chat_settings.
func (s *Store) GetSetting(ctx context.Context, key string, out any) error {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM chat_settings WHERE key = $1`, key).Scan(&raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// SetSetting записывает настройку в таблицу chat_settings (Zero-ENV).
func (s *Store) SetSetting(ctx context.Context, key string, val any) error {
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO chat_settings (key, value, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = NOW()
	`, key, raw)
	return err
}

// ListChatMembers возвращает список всех участников чата с ролями
func (s *Store) ListChatMembers(ctx context.Context, chatID uuid.UUID) ([]ChatMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT chat_id, user_id, COALESCE(display_name, ''), role, joined_at
		FROM chat_members
		WHERE chat_id = $1
		ORDER BY role = 'admin' DESC, role = 'moderator' DESC, joined_at ASC
	`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []ChatMember
	for rows.Next() {
		var m ChatMember
		if err := rows.Scan(&m.ChatID, &m.UserID, &m.DisplayName, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	if members == nil {
		members = []ChatMember{}
	}
	return members, rows.Err()
}

// AddOrUpdateChatMember добавляет участника или обновляет его роль и отображаемое имя
func (s *Store) AddOrUpdateChatMember(ctx context.Context, chatID uuid.UUID, userID, displayName, role string) error {
	if role == "" {
		role = MemberRoleMember
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO chat_members (chat_id, user_id, display_name, role, joined_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (chat_id, user_id) DO UPDATE
		SET role = EXCLUDED.role,
		    display_name = CASE WHEN EXCLUDED.display_name <> '' THEN EXCLUDED.display_name ELSE chat_members.display_name END
	`, chatID, userID, displayName, role)
	return err
}

// RemoveChatMember удаляет участника из чата
func (s *Store) RemoveChatMember(ctx context.Context, chatID uuid.UUID, userID string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM chat_members
		WHERE chat_id = $1 AND user_id = $2
	`, chatID, userID)
	return err
}

// ListMissedMessagesForUser возвращает список пропущенных сообщений с id > sinceID для чатов пользователя
func (s *Store) ListMissedMessagesForUser(ctx context.Context, userID string, groups []string, dept, role string, sinceID int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	isAdmin := role == "chat_admin" || role == "admin" || role == "superadmin"

	query := `
		SELECT m.id, m.chat_id, m.topic_id, m.sender_id, m.sender_name, m.client_msg_id, m.reply_to_id, m.body, m.kind, m.secret_ttl_seconds, m.secret_read_at, m.metadata, m.created_at, m.edited_at, m.deleted_at
		FROM messages m
		JOIN chats c ON c.id = m.chat_id
		WHERE m.id > $1
		  AND m.deleted_at IS NULL
		  AND c.is_archived = FALSE
		  AND (
		      $3 = TRUE
		      OR EXISTS (SELECT 1 FROM chat_members cm WHERE cm.chat_id = c.id AND cm.user_id = $2)
		      OR EXISTS (
		          SELECT 1 FROM group_access_rules gar
		          WHERE gar.chat_id = c.id AND (
		              (gar.rule_type = 'ad_group' AND gar.target_name = ANY($4))
		              OR (gar.rule_type = 'department' AND gar.target_name = $5)
		              OR (gar.rule_type = 'user' AND gar.target_name = $2)
		          )
		      )
		  )
		ORDER BY m.id ASC
		LIMIT $6
	`

	rows, err := s.pool.Query(ctx, query, sinceID, userID, isAdmin, groups, dept, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	var msgIDs []int64
	for rows.Next() {
		var m Message
		var metaRaw []byte
		if err := rows.Scan(&m.ID, &m.ChatID, &m.TopicID, &m.SenderID, &m.SenderName, &m.ClientMsgID, &m.ReplyToID, &m.Body, &m.Kind, &m.SecretTTLSeconds, &m.SecretReadAt, &metaRaw, &m.CreatedAt, &m.EditedAt, &m.DeletedAt); err != nil {
			return nil, err
		}
		if len(metaRaw) > 0 {
			_ = json.Unmarshal(metaRaw, &m.Metadata)
		}
		msgs = append(msgs, m)
		msgIDs = append(msgIDs, m.ID)
	}

	if len(msgIDs) > 0 {
		attRows, aErr := s.pool.Query(ctx, `
			SELECT id, message_id, chat_id, file_name, file_size, content_type, storage_path, sha256_hex, created_at
			FROM attachments
			WHERE message_id = ANY($1)
		`, msgIDs)
		if aErr == nil {
			defer attRows.Close()
			attsByMsg := make(map[int64][]Attachment)
			for attRows.Next() {
				var a Attachment
				if err := attRows.Scan(&a.ID, &a.MessageID, &a.ChatID, &a.FileName, &a.FileSize, &a.ContentType, &a.StoragePath, &a.SHA256Hex, &a.CreatedAt); err == nil {
					attsByMsg[a.MessageID] = append(attsByMsg[a.MessageID], a)
				}
			}
			for i := range msgs {
				if attList, ok := attsByMsg[msgs[i].ID]; ok {
					if msgs[i].Metadata == nil {
						msgs[i].Metadata = make(map[string]any)
					}
					msgs[i].Metadata["attachments"] = attList
				}
			}
		}
	}

	if msgs == nil {
		msgs = []Message{}
	}
	return msgs, rows.Err()
}

func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
