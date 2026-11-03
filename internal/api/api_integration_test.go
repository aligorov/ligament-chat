//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aligorov/ligament-chat/internal/contract"
	"github.com/aligorov/ligament-chat/internal/store"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func getTestPostgresDSN() string {
	dsn := os.Getenv("CHAT_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://postgres@127.0.0.1:57368/ligament_chat_test?sslmode=disable"
	}
	return dsn
}

func setupTestEnvironment(t *testing.T) (*Server, *store.Store, ed25519.PublicKey, ed25519.PrivateKey, uuid.UUID) {
	t.Helper()

	st := store.NewStore()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dsn := getTestPostgresDSN()
	if err := st.AttachPostgres(ctx, dsn); err != nil {
		t.Skipf("skipping integration test, postgres not available at %s: %v", dsn, err)
	}

	corePub, corePriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	coreInstanceID := uuid.New()

	// Регистрация доверия в БД
	if err := st.SaveTrust(ctx, coreInstanceID.String(), hex.EncodeToString(corePub), "default"); err != nil {
		t.Fatalf("SaveTrust: %v", err)
	}

	hub := NewHub()
	stopHub := make(chan struct{})
	go hub.Run(stopHub)
	t.Cleanup(func() {
		close(stopHub)
		st.Close()
	})

	srv := NewServer(st, hub, t.TempDir(), "", "http://127.0.0.1:8080")
	return srv, st, corePub, corePriv, coreInstanceID
}

func makeToken(t *testing.T, coreID uuid.UUID, priv ed25519.PrivateKey, userID, username, role string, adGroups []string) string {
	t.Helper()
	now := time.Now().UTC()
	claims := contract.ChatClaims{
		Issuer:      coreID.String(),
		Audience:    contract.AudienceChat,
		Subject:     userID,
		Username:    username,
		ActorName:   username,
		Role:        role,
		ADGroups:    adGroups,
		IssuedAt:    now.Unix(),
		NotBefore:   now.Unix(),
		ExpiresAt:   now.Add(5 * time.Minute).Unix(),
		TenantScope: "default",
	}
	tok, err := contract.SignChatToken(claims, priv)
	if err != nil {
		t.Fatalf("makeToken: %v", err)
	}
	return tok
}

func TestIntegration_TrustHandshake(t *testing.T) {
	srv, _, _, _, _ := setupTestEnvironment(t)

	newCorePub, newCorePriv, _ := ed25519.GenerateKey(rand.Reader)
	newInstanceID := uuid.New()
	pubHex := hex.EncodeToString(newCorePub)
	now := time.Now().UTC().Unix()
	tenant := "default"

	msg := fmt.Sprintf("ligament-chat-trust:%s:%s:%s:%d", newInstanceID.String(), pubHex, tenant, now)
	sig := ed25519.Sign(newCorePriv, []byte(msg))

	body, _ := json.Marshal(map[string]any{
		"instance_id":    newInstanceID.String(),
		"public_key_hex": pubHex,
		"tenant_scope":   tenant,
		"timestamp":      now,
		"signature":      hex.EncodeToString(sig),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/trust", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIntegration_Chat_Forum_Topics_HighWaterMark(t *testing.T) {
	srv, _, _, corePriv, coreID := setupTestEnvironment(t)

	ivanovToken := makeToken(t, coreID, corePriv, "user-ivanov", "ivanov", "chat_user", []string{"IT"})
	petrovToken := makeToken(t, coreID, corePriv, "user-petrov", "petrov", "chat_user", []string{"IT"})

	// 1. Создание форума-чата "ИТ Отдел"
	chatBody, _ := json.Marshal(map[string]any{
		"kind":        "group",
		"title":       "ИТ Отдел",
		"description": "Форум ИТ-департамента",
		"is_forum":    true,
		"access_rules": []map[string]any{
			{
				"rule_type":   "ad_group",
				"target_name": "IT",
				"can_write":   true,
				"can_call":    true,
			},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chats", bytes.NewReader(chatBody))
	req.Header.Set("Authorization", "Bearer "+ivanovToken)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create chat: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var createdChat store.Chat
	_ = json.Unmarshal(w.Body.Bytes(), &createdChat)

	// 2. Проверяем, что в форуме автоматически создан топик "Общий"
	req = httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+createdChat.ID.String()+"/topics", nil)
	req.Header.Set("Authorization", "Bearer "+ivanovToken)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list topics: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var topics []store.ChatTopic
	_ = json.Unmarshal(w.Body.Bytes(), &topics)
	if len(topics) != 1 || topics[0].Title != "Общий" {
		t.Fatalf("expected 1 general topic 'Общий', got %+v", topics)
	}

	// 3. Создание топика "Серверы" админом чата (ivanov)
	topicBody, _ := json.Marshal(map[string]any{
		"title":      "Серверы",
		"icon_emoji": "🖥️",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+createdChat.ID.String()+"/topics", bytes.NewReader(topicBody))
	req.Header.Set("Authorization", "Bearer "+ivanovToken)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create topic: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var serverTopic store.ChatTopic
	_ = json.Unmarshal(w.Body.Bytes(), &serverTopic)

	// 4. Иванов отправляет сообщение в топик "Серверы"
	msgBody, _ := json.Marshal(map[string]any{
		"topic_id": serverTopic.ID,
		"body":     "Перезагрузка ноды proxmox-01 запланирована на 22:00",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+createdChat.ID.String()+"/messages", bytes.NewReader(msgBody))
	req.Header.Set("Authorization", "Bearer "+ivanovToken)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("send message: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var sentMsg store.Message
	_ = json.Unmarshal(w.Body.Bytes(), &sentMsg)

	// 5. Петров запрашивает топики: должно быть unread_count == 1 у топика "Серверы"
	req = httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+createdChat.ID.String()+"/topics", nil)
	req.Header.Set("Authorization", "Bearer "+petrovToken)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	var petrovTopics []store.ChatTopic
	_ = json.Unmarshal(w.Body.Bytes(), &petrovTopics)
	var foundServerTopic *store.ChatTopic
	for _, tp := range petrovTopics {
		if tp.ID == serverTopic.ID {
			foundServerTopic = &tp
			break
		}
	}
	if foundServerTopic == nil || foundServerTopic.UnreadCount != 1 {
		t.Fatalf("expected petrov unread_count == 1 for Servers, got %+v", foundServerTopic)
	}

	// 6. Петров читает сообщение (сдвигает указатель High-Water Mark)
	readBody, _ := json.Marshal(map[string]any{
		"topic_id":     serverTopic.ID,
		"last_read_id": sentMsg.ID,
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+createdChat.ID.String()+"/read", bytes.NewReader(readBody))
	req.Header.Set("Authorization", "Bearer "+petrovToken)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("read pointer: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 7. Петров снова запрашивает топики: unread_count должен стать 0!
	req = httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+createdChat.ID.String()+"/topics", nil)
	req.Header.Set("Authorization", "Bearer "+petrovToken)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	var updatedTopics []store.ChatTopic
	_ = json.Unmarshal(w.Body.Bytes(), &updatedTopics)
	for _, tp := range updatedTopics {
		if tp.ID == serverTopic.ID {
			if tp.UnreadCount != 0 {
				t.Fatalf("expected unread_count == 0 after reading, got %d", tp.UnreadCount)
			}
		}
	}
}

func TestIntegration_WebSocket_Realtime_Broadcast(t *testing.T) {
	srv, _, _, corePriv, coreID := setupTestEnvironment(t)
	httpServer := httptest.NewServer(srv)
	defer httpServer.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/api/v1/ws"

	token := makeToken(t, coreID, corePriv, "user-alice", "alice", "chat_user", []string{"all"})
	ws, _, err := websocket.DefaultDialer.Dial(wsURL+"?token="+token, nil)
	if err != nil {
		t.Fatalf("Dial WS failed: %v", err)
	}
	defer ws.Close()

	chatID := uuid.New()
	// Присоединяемся к комнате чата
	joinMsg := HubMessage{
		Type:   "room.join",
		ChatID: chatID.String(),
	}
	if err := ws.WriteJSON(joinMsg); err != nil {
		t.Fatalf("WriteJSON room.join: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Шлём broadcast через Hub
	srv.hub.BroadcastToRoom("chat:"+chatID.String(), HubMessage{
		Type:    "chat.message",
		ChatID:  chatID.String(),
		TopicID: 42,
		Payload: map[string]any{"text": "Hello WebSocket!"},
	})

	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	var received HubMessage
	if err := ws.ReadJSON(&received); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}

	if received.Type != "chat.message" || received.ChatID != chatID.String() {
		t.Fatalf("unexpected message received: %+v", received)
	}
	if text, ok := received.Payload["text"].(string); !ok || text != "Hello WebSocket!" {
		t.Fatalf("payload text mismatch: %+v", received.Payload)
	}
}
