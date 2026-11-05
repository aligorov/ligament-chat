package store

import (
	"time"

	"github.com/google/uuid"
)

// Роли участников в чате
const (
	MemberRoleAdmin     = "admin"
	MemberRoleModerator = "moderator"
	MemberRoleMember    = "member"
	MemberRoleReadOnly  = "read_only"
)

// ChatMember представляет участника чата с ролью
type ChatMember struct {
	ChatID      uuid.UUID `json:"chat_id"`
	UserID      string    `json:"user_id"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	JoinedAt    time.Time `json:"joined_at"`
}

// Типы чатов
const (
	ChatKindDirect  = "direct"
	ChatKindGroup   = "group"
	ChatKindChannel = "channel"
	ChatKindSaved   = "saved"
)

// Типы сообщений
const (
	MsgKindText       = "text"
	MsgKindSecret     = "secret"      // One-time self-destructing secret
	MsgKindActionCard = "action_card"  // 2FA approval card
	MsgKindLinkCall   = "link_call"    // WebRTC call status / invite
	MsgKindSystem     = "system"
)

// Chat представляет чат или форум
type Chat struct {
	ID          uuid.UUID `json:"id"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	AvatarURL   string    `json:"avatar_url"`
	IsForum     bool      `json:"is_forum"`
	IsArchived  bool      `json:"is_archived"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	UnreadCount int       `json:"unread_count"`
	PeerUserID  string    `json:"peer_user_id,omitempty"`
}

// ChatTopic представляет топик (тему) в форуме
type ChatTopic struct {
	ID          int64     `json:"id"`
	ChatID      uuid.UUID `json:"chat_id"`
	Title       string    `json:"title"`
	IconEmoji   string    `json:"icon_emoji"`
	IsGeneral   bool      `json:"is_general"`
	IsClosed    bool      `json:"is_closed"`
	IsPinned    bool      `json:"is_pinned"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	UnreadCount int       `json:"unread_count"`
}

// ReplyPreview содержит превью цитируемого сообщения
type ReplyPreview struct {
	ID         int64  `json:"id"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Body       string `json:"body"`
}

// Message представляет отдельное сообщение
type Message struct {
	ID               int64          `json:"id"`
	ChatID           uuid.UUID      `json:"chat_id"`
	TopicID          int64          `json:"topic_id"`
	SenderID         string         `json:"sender_id"`
	SenderName       string         `json:"sender_name"`
	ClientMsgID      string         `json:"client_msg_id"`
	ReplyToID        *int64         `json:"reply_to_id,omitempty"`
	ReplyTo          *ReplyPreview  `json:"reply_to,omitempty"`
	Body             string         `json:"body"`
	Kind             string         `json:"kind"`
	SecretTTLSeconds *int           `json:"secret_ttl_seconds,omitempty"`
	SecretReadAt     *time.Time     `json:"secret_read_at,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	EditedAt         *time.Time     `json:"edited_at,omitempty"`
	DeletedAt        *time.Time     `json:"deleted_at,omitempty"`
	Attachments      []Attachment   `json:"attachments,omitempty"`
}

// Attachment представляет вложенный файл
type Attachment struct {
	ID          uuid.UUID `json:"id"`
	MessageID   *int64    `json:"message_id,omitempty"`
	ChatID      uuid.UUID `json:"chat_id"`
	FileName    string    `json:"file_name"`
	FileSize    int64     `json:"file_size"`
	ContentType string    `json:"content_type"`
	StoragePath string    `json:"storage_path"`
	SHA256Hex   string    `json:"sha256_hex"`
	CreatedAt   time.Time `json:"created_at"`
}

// GroupAccessRule связывает чат с AD-группой, отделом или пользователем
type GroupAccessRule struct {
	ID          uuid.UUID `json:"id"`
	ChatID      uuid.UUID `json:"chat_id"`
	RuleType    string    `json:"rule_type"` // ad_group, department, role, user
	TargetName  string    `json:"target_name"`
	CanWrite    bool      `json:"can_write"`
	CanCall     bool      `json:"can_call"`
	IsModerator bool      `json:"is_moderator"`
	CreatedAt   time.Time `json:"created_at"`
}

// CallSession описывает сеанс WebRTC звонка
type CallSession struct {
	ID                uuid.UUID  `json:"id"`
	ChatID            uuid.UUID  `json:"chat_id"`
	CallerUserID      string     `json:"caller_user_id"`
	CalleeUserID      string     `json:"callee_user_id"`
	Status            string     `json:"status"` // ringing, active, ended, rejected, missed
	IsRecorded        bool       `json:"is_recorded"`
	RecorderSessionID *uuid.UUID `json:"recorder_session_id,omitempty"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	EndedAt           *time.Time `json:"ended_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

// UserPresence хранит статус присутствия сотрудника в чате (онлайн, занят, в отпуске и др.)
type UserPresence struct {
	UserID     string    `json:"user_id"`
	Status     string    `json:"status"` // online, busy, vacation, away, dnd, offline
	StatusText string    `json:"status_text"`
	IsOnline   bool      `json:"is_online"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// UnreadUserSummary содержит агрегированные данные о непрочитанных сообщениях пользователя для push-напоминаний.
type UnreadUserSummary struct {
	UserID        string    `json:"user_id"`
	TotalUnread   int       `json:"total_unread"`
	ChatsCount    int       `json:"chats_count"`
	MaxMessageID  int64     `json:"max_message_id"`
	LastMessageAt time.Time `json:"last_message_at"`
	ChatID        uuid.UUID `json:"chat_id"`
	ChatTitle     string    `json:"chat_title"`
	SenderName    string    `json:"sender_name"`
	MessageBody   string    `json:"message_body"`
}
