package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/aligorov/ligament-chat/internal/store"
	"github.com/google/uuid"
)

// handleListChats — GET /api/v1/chats
func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	chats, err := s.st.ListUserChats(r.Context(), claims.Subject, claims.ADGroups, claims.Department, claims.Role)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_list_chats"})
		return
	}
	if chats == nil {
		chats = []store.Chat{}
	}
	writeJSON(w, http.StatusOK, chats)
}

type createChatReq struct {
	Kind           string                  `json:"kind"` // direct, group, channel
	Title          string                  `json:"title"`
	Description    string                  `json:"description"`
	AvatarURL      string                  `json:"avatar_url"`
	IsForum        bool                    `json:"is_forum"`
	InitialMembers []string                `json:"initial_members"`
	AccessRules    []store.GroupAccessRule `json:"access_rules"`
}

// handleCreateChat — POST /api/v1/chats
func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	var req createChatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	req.Title = strings.TrimSpace(req.Title)
	if req.Kind == "" {
		req.Kind = store.ChatKindGroup
	}
	if req.Kind != store.ChatKindDirect && req.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty_chat_title"})
		return
	}

	chat := &store.Chat{
		ID:          uuid.New(),
		Kind:        req.Kind,
		Title:       req.Title,
		Description: req.Description,
		AvatarURL:   req.AvatarURL,
		IsForum:     req.IsForum,
		CreatedBy:   claims.Subject,
	}

	if err := s.st.CreateChat(r.Context(), chat, req.InitialMembers, req.AccessRules); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_create_chat"})
		return
	}

	writeJSON(w, http.StatusCreated, chat)
}

// handleGetChat — GET /api/v1/chats/{id}
func (s *Server) handleGetChat(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	chatIDStr := r.PathValue("id")
	chatID, err := uuid.Parse(chatIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_chat_id"})
		return
	}

	ok, _, err := s.st.CanAccessChat(r.Context(), chatID, claims.Subject, claims.ADGroups, claims.Department, claims.Role)
	if err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access_denied"})
		return
	}

	chat, err := s.st.GetChat(r.Context(), chatID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chat_not_found"})
		return
	}

	writeJSON(w, http.StatusOK, chat)
}

// handleListTopics — GET /api/v1/chats/{id}/topics
func (s *Server) handleListTopics(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	chatIDStr := r.PathValue("id")
	chatID, err := uuid.Parse(chatIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_chat_id"})
		return
	}

	ok, _, err := s.st.CanAccessChat(r.Context(), chatID, claims.Subject, claims.ADGroups, claims.Department, claims.Role)
	if err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access_denied"})
		return
	}

	topics, err := s.st.ListChatTopics(r.Context(), chatID, claims.Subject)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_list_topics"})
		return
	}
	if topics == nil {
		topics = []store.ChatTopic{}
	}

	writeJSON(w, http.StatusOK, topics)
}

type createTopicReq struct {
	Title     string `json:"title"`
	IconEmoji string `json:"icon_emoji"`
}

// handleCreateTopic — POST /api/v1/chats/{id}/topics
func (s *Server) handleCreateTopic(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	chatIDStr := r.PathValue("id")
	chatID, err := uuid.Parse(chatIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_chat_id"})
		return
	}

	ok, role, err := s.st.CanAccessChat(r.Context(), chatID, claims.Subject, claims.ADGroups, claims.Department, claims.Role)
	if err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access_denied"})
		return
	}
	if role != store.MemberRoleAdmin && role != store.MemberRoleModerator {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "moderator_or_admin_required"})
		return
	}

	var req createTopicReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty_topic_title"})
		return
	}
	if req.IconEmoji == "" {
		req.IconEmoji = "💬"
	}

	topic := &store.ChatTopic{
		ChatID:    chatID,
		Title:     req.Title,
		IconEmoji: req.IconEmoji,
		CreatedBy: claims.Subject,
	}

	if err := s.st.CreateTopic(r.Context(), topic); err != nil {
		if store.IsUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "topic_already_exists"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_create_topic"})
		return
	}

	writeJSON(w, http.StatusCreated, topic)
}

// handleListMessages — GET /api/v1/chats/{id}/messages
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	chatIDStr := r.PathValue("id")
	chatID, err := uuid.Parse(chatIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_chat_id"})
		return
	}

	ok, _, err := s.st.CanAccessChat(r.Context(), chatID, claims.Subject, claims.ADGroups, claims.Department, claims.Role)
	if err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access_denied"})
		return
	}

	q := r.URL.Query()
	var topicID int64
	if tStr := q.Get("topic_id"); tStr != "" {
		topicID, _ = strconv.ParseInt(tStr, 10, 64)
	}
	var beforeID int64
	if bStr := q.Get("before_id"); bStr != "" {
		beforeID, _ = strconv.ParseInt(bStr, 10, 64)
	}
	limit := 50
	if lStr := q.Get("limit"); lStr != "" {
		if parsed, err := strconv.Atoi(lStr); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}

	msgs, err := s.st.ListMessages(r.Context(), chatID, topicID, beforeID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_list_messages"})
		return
	}
	if msgs == nil {
		msgs = []store.Message{}
	}

	writeJSON(w, http.StatusOK, msgs)
}

type sendMessageReq struct {
	TopicID          int64          `json:"topic_id"`
	ClientMsgID      string         `json:"client_msg_id"`
	ReplyToID        *int64         `json:"reply_to_id"`
	Body             string         `json:"body"`
	Kind             string         `json:"kind"`
	SecretTTLSeconds *int           `json:"secret_ttl_seconds"`
	Metadata         map[string]any `json:"metadata"`
}

// handleSendMessage — POST /api/v1/chats/{id}/messages
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	chatIDStr := r.PathValue("id")
	chatID, err := uuid.Parse(chatIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_chat_id"})
		return
	}

	ok, role, err := s.st.CanAccessChat(r.Context(), chatID, claims.Subject, claims.ADGroups, claims.Department, claims.Role)
	if err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access_denied"})
		return
	}
	if role == "viewer" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read_only_mode"})
		return
	}

	var req sendMessageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	req.Body = strings.TrimSpace(req.Body)
	if req.Body == "" && req.Kind != store.MsgKindLinkCall {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty_body"})
		return
	}
	if req.Kind == "" {
		req.Kind = store.MsgKindText
	}
	clientMsgID := strings.TrimSpace(req.ClientMsgID)
	if clientMsgID == "" {
		clientMsgID = uuid.NewString()
	}

	senderName := claims.ActorName
	if senderName == "" {
		senderName = claims.Username
	}

	msg := &store.Message{
		ChatID:           chatID,
		TopicID:          req.TopicID,
		SenderID:         claims.Subject,
		SenderName:       senderName,
		ClientMsgID:      clientMsgID,
		ReplyToID:        req.ReplyToID,
		Body:             req.Body,
		Kind:             req.Kind,
		SecretTTLSeconds: req.SecretTTLSeconds,
		Metadata:         req.Metadata,
	}

	if err := s.st.SendMessage(r.Context(), msg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_send_message"})
		return
	}

	// Отправитель автоматически сдвигает свой read pointer на отправленное сообщение
	_ = s.st.UpdateReadPointer(r.Context(), chatID, req.TopicID, claims.Subject, msg.ID)

	// Broadcast нового сообщения в WebSocket комнату
	s.hub.BroadcastToRoom("chat:"+chatID.String(), HubMessage{
		Type:    "chat.message",
		ChatID:  chatID.String(),
		TopicID: req.TopicID,
		Payload: map[string]any{
			"message": msg,
		},
	})

	writeJSON(w, http.StatusCreated, msg)
}

type readPointerReq struct {
	TopicID    int64 `json:"topic_id"`
	LastReadID int64 `json:"last_read_id"`
}

// handleReadPointer — POST /api/v1/chats/{id}/read
func (s *Server) handleReadPointer(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	chatIDStr := r.PathValue("id")
	chatID, err := uuid.Parse(chatIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_chat_id"})
		return
	}

	var req readPointerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	if err := s.st.UpdateReadPointer(r.Context(), chatID, req.TopicID, claims.Subject, req.LastReadID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_update_read_pointer"})
		return
	}

	// Трансляция перемещения указателя прочтения в WebSocket комнату
	s.hub.BroadcastToRoom("chat:"+chatID.String(), HubMessage{
		Type:    "chat.read_pointer",
		ChatID:  chatID.String(),
		TopicID: req.TopicID,
		Payload: map[string]any{
			"user_id":      claims.Subject,
			"last_read_id": req.LastReadID,
		},
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
