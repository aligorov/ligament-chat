package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aligorov/ligament-chat/internal/contract"
	"github.com/aligorov/ligament-chat/internal/store"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 65536
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true // Same origin checks handled by Core proxy / token verification
	},
}

// HubMessage представляет сообщение WebSocket протокола
type HubMessage struct {
	Type     string         `json:"type"`                // chat.message, chat.read, chat.typing, webrtc.signal, call.invite, etc.
	ChatID   string         `json:"chat_id,omitempty"`
	TopicID  int64          `json:"topic_id,omitempty"`
	TargetID string         `json:"target_id,omitempty"` // callee / target user ID
	Payload  map[string]any `json:"payload,omitempty"`
}

type Client struct {
	hub      *Hub
	st       *store.Store
	claims   *contract.ChatClaims
	conn     *websocket.Conn
	userID   string
	send     chan []byte
	rooms    map[string]bool
	roomsMu  sync.RWMutex
}

type Hub struct {
	mu            sync.RWMutex
	st            *store.Store
	clients       map[*Client]bool
	userClients   map[string]map[*Client]bool
	roomClients   map[string]map[*Client]bool
	userPresences map[string]*store.UserPresence
	pendingCalls  map[string]*PendingCall // targetUserID -> PendingCall
	coreURL       string
	register      chan *Client
	unregister    chan *Client
	broadcast     chan *broadcastEvent
	direct        chan *directEvent
}

// PendingCall представляет отложенный входящий вызов (когда адресат временно не в сети / в фоне)
type PendingCall struct {
	CallerID  string
	TargetID  string
	ChatID    string
	TopicID   int64
	Payload   map[string]any
	CreatedAt time.Time
	ExpiresAt time.Time
	Timer     *time.Timer
}

type broadcastEvent struct {
	room string
	data []byte
}

type directEvent struct {
	targetUserID string
	data         []byte
}

func NewHub(st ...*store.Store) *Hub {
	h := &Hub{
		clients:       make(map[*Client]bool),
		userClients:   make(map[string]map[*Client]bool),
		roomClients:   make(map[string]map[*Client]bool),
		userPresences: make(map[string]*store.UserPresence),
		pendingCalls:  make(map[string]*PendingCall),
		register:      make(chan *Client),
		unregister:    make(chan *Client),
		broadcast:     make(chan *broadcastEvent, 256),
		direct:        make(chan *directEvent, 256),
	}
	if len(st) > 0 && st[0] != nil {
		h.st = st[0]
		if list, err := h.st.ListUserPresences(context.Background()); err == nil {
			h.userPresences = list
		}
	}
	return h
}

func (h *Hub) SetStore(st *store.Store) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.st = st
	if st != nil {
		if list, err := st.ListUserPresences(context.Background()); err == nil {
			for k, v := range list {
				h.userPresences[k] = v
			}
		}
	}
}

func (h *Hub) unregisterClientLocked(client *Client) {
	if _, ok := h.clients[client]; !ok {
		return
	}
	delete(h.clients, client)
	close(client.send)
	if uMap, ok := h.userClients[client.userID]; ok {
		delete(uMap, client)
		if len(uMap) == 0 {
			delete(h.userClients, client.userID)
			// Если у пользователя не осталось активных сессий, отменяем инициированные им ожидающие звонки
			for targetID, pc := range h.pendingCalls {
				if pc.CallerID == client.userID {
					if pc.Timer != nil {
						pc.Timer.Stop()
					}
					delete(h.pendingCalls, targetID)
				}
			}
		}
	}
	client.roomsMu.RLock()
	for room := range client.rooms {
		if rMap, ok := h.roomClients[room]; ok {
			delete(rMap, client)
			if len(rMap) == 0 {
				delete(h.roomClients, room)
			}
		}
	}
	client.roomsMu.RUnlock()
}

// EvictUser немедленно завершает все активные WebSocket соединения пользователя (отзыв доступа).
func (h *Hub) EvictUser(userID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var targets []*Client
	for client := range h.userClients[userID] {
		targets = append(targets, client)
	}
	for _, client := range targets {
		h.unregisterClientLocked(client)
		_ = client.conn.Close()
	}
}

// CreatePendingCall ставит звонок в буфер ожидания пробуждения абонента
func (h *Hub) CreatePendingCall(callerID, targetID, chatID string, topicID int64, payload map[string]any, ttl time.Duration, onTimeout func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Гасим предыдущий таймер, если для этого адресата уже был ожидающий вызов
	if old, exists := h.pendingCalls[targetID]; exists && old.Timer != nil {
		old.Timer.Stop()
	}

	pc := &PendingCall{
		CallerID:  callerID,
		TargetID:  targetID,
		ChatID:    chatID,
		TopicID:   topicID,
		Payload:   payload,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
	}

	pc.Timer = time.AfterFunc(ttl, func() {
		h.mu.Lock()
		cur, exists := h.pendingCalls[targetID]
		if exists && cur == pc {
			delete(h.pendingCalls, targetID)
		}
		h.mu.Unlock()

		if exists && cur == pc && onTimeout != nil {
			onTimeout()
		}
	})

	h.pendingCalls[targetID] = pc
}

// CancelPendingCall отменяет ожидающий вызов между звонящим и адресатом
func (h *Hub) CancelPendingCall(callerID, targetID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if pc, ok := h.pendingCalls[targetID]; ok && pc.CallerID == callerID {
		if pc.Timer != nil {
			pc.Timer.Stop()
		}
		delete(h.pendingCalls, targetID)
	}
}

// notifyCoreCall уведомляет Core о звонке временно отключенному абоненту для отправки push
func (h *Hub) notifyCoreCall(callerID, targetID, chatID string, isVideo bool) {
	h.mu.RLock()
	coreURL := h.coreURL
	h.mu.RUnlock()

	if coreURL == "" {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"caller":      callerID,
		"target_user": targetID,
		"chat_id":     chatID,
		"is_video":    isVideo,
	})
	if err != nil {
		return
	}
	endpoint := strings.TrimRight(coreURL, "/") + "/apps/chat/api/v1/internal/notify-call"
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err == nil && resp != nil {
		_ = resp.Body.Close()
	}
}

func (h *Hub) Run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			h.mu.Lock()
			for c := range h.clients {
				close(c.send)
				_ = c.conn.Close()
			}
			h.clients = make(map[*Client]bool)
			h.userClients = make(map[string]map[*Client]bool)
			h.roomClients = make(map[string]map[*Client]bool)
			h.mu.Unlock()
			return

		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			if _, ok := h.userClients[client.userID]; !ok {
				h.userClients[client.userID] = make(map[*Client]bool)
			}
			prevCount := len(h.userClients[client.userID])
			h.userClients[client.userID][client] = true
			userRoom := "user:" + client.userID
			if _, ok := h.roomClients[userRoom]; !ok {
				h.roomClients[userRoom] = make(map[*Client]bool)
			}
			h.roomClients[userRoom][client] = true
			client.roomsMu.Lock()
			client.rooms[userRoom] = true
			client.roomsMu.Unlock()

			firstConnect := (prevCount == 0)
			p := h.effectivePresenceLocked(client.userID)

			// Проверяем наличие отложенного входящего вызова для подключившегося абонента
			var pending *PendingCall
			if pc, ok := h.pendingCalls[client.userID]; ok && time.Now().Before(pc.ExpiresAt) {
				pending = pc
				delete(h.pendingCalls, client.userID)
				if pc.Timer != nil {
					pc.Timer.Stop()
				}
			}
			h.mu.Unlock()

			if pending != nil {
				// Немедленно доставляем WebRTC offer новому подключению адресата
				client.sendDirect(HubMessage{
					Type:     "webrtc.signal",
					ChatID:   pending.ChatID,
					TopicID:  pending.TopicID,
					TargetID: pending.CallerID,
					Payload:  pending.Payload,
				})
			}

			if firstConnect {
				h.BroadcastAll(HubMessage{
					Type: "presence.update",
					Payload: map[string]any{
						"presence": p,
					},
				})
			}

		case client := <-h.unregister:
			h.mu.Lock()
			h.unregisterClientLocked(client)
			userLeft := (len(h.userClients[client.userID]) == 0)
			p := h.effectivePresenceLocked(client.userID)
			h.mu.Unlock()

			if userLeft {
				h.BroadcastAll(HubMessage{
					Type: "presence.update",
					Payload: map[string]any{
						"presence": p,
					},
				})
			}

		case ev := <-h.broadcast:
			h.mu.RLock()
			var deadClients []*Client
			for client := range h.roomClients[ev.room] {
				select {
				case client.send <- ev.data:
				default:
					deadClients = append(deadClients, client)
				}
			}
			h.mu.RUnlock()

			if len(deadClients) > 0 {
				h.mu.Lock()
				for _, client := range deadClients {
					h.unregisterClientLocked(client)
				}
				h.mu.Unlock()
			}

		case ev := <-h.direct:
			h.mu.RLock()
			var deadClients []*Client
			for client := range h.userClients[ev.targetUserID] {
				select {
				case client.send <- ev.data:
				default:
					deadClients = append(deadClients, client)
				}
			}
			h.mu.RUnlock()

			if len(deadClients) > 0 {
				h.mu.Lock()
				for _, client := range deadClients {
					h.unregisterClientLocked(client)
				}
				h.mu.Unlock()
			}
		}
	}
}

// BroadcastToRoom отправляет событие всем подписчикам комнаты (чата или топика)
func (h *Hub) BroadcastToRoom(room string, msg HubMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.broadcast <- &broadcastEvent{
		room: room,
		data: data,
	}
}

// IsUserOnline проверяет, подключен ли пользователь хотя бы одним активным клиентом WebSocket
func (h *Hub) IsUserOnline(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.userClients[userID]) > 0
}

// SendDirect отправляет событие конкретному пользователю на все его клиенты (P2P сигналинг звонков)
func (h *Hub) SendDirect(targetUserID string, msg HubMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.direct <- &directEvent{
		targetUserID: targetUserID,
		data:         data,
	}
}

// BroadcastAll рассылает сообщение всем подключенным клиентам чата
func (h *Hub) BroadcastAll(msg HubMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	var deadClients []*Client
	for client := range h.clients {
		select {
		case client.send <- data:
		default:
			deadClients = append(deadClients, client)
		}
	}
	h.mu.RUnlock()

	if len(deadClients) > 0 {
		h.mu.Lock()
		for _, client := range deadClients {
			h.unregisterClientLocked(client)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) effectivePresenceLocked(userID string) store.UserPresence {
	isOnline := len(h.userClients[userID]) > 0
	custom, ok := h.userPresences[userID]
	if ok && custom != nil {
		p := *custom
		p.IsOnline = isOnline
		if p.Status == "" {
			if isOnline {
				p.Status = "online"
			} else {
				p.Status = "offline"
			}
		}
		return p
	}

	st := "offline"
	if isOnline {
		st = "online"
	}
	return store.UserPresence{
		UserID:    userID,
		Status:    st,
		IsOnline:  isOnline,
		UpdatedAt: time.Now().UTC(),
	}
}

// UpdateUserPresence обновляет статус сотрудника (в сети, занят, в отпуске и др.)
func (h *Hub) UpdateUserPresence(ctx context.Context, userID, status, statusText string) store.UserPresence {
	h.mu.Lock()
	if h.st != nil {
		_, _ = h.st.SetUserPresence(ctx, userID, status, statusText)
	}
	now := time.Now().UTC()
	h.userPresences[userID] = &store.UserPresence{
		UserID:     userID,
		Status:     status,
		StatusText: statusText,
		UpdatedAt:  now,
	}
	p := h.effectivePresenceLocked(userID)
	h.mu.Unlock()

	h.BroadcastAll(HubMessage{
		Type: "presence.update",
		Payload: map[string]any{
			"presence": p,
		},
	})
	return p
}

// GetPresencesSnapshot возвращает снимок статусов всех пользователей
func (h *Hub) GetPresencesSnapshot() []store.UserPresence {
	h.mu.RLock()
	defer h.mu.RUnlock()

	allUsers := make(map[string]bool)
	for u := range h.userClients {
		if len(h.userClients[u]) > 0 {
			allUsers[u] = true
		}
	}
	for u := range h.userPresences {
		allUsers[u] = true
	}

	res := make([]store.UserPresence, 0, len(allUsers))
	for u := range allUsers {
		res = append(res, h.effectivePresenceLocked(u))
	}
	return res
}

// GetUserPresence возвращает текущий статус сотрудника
func (h *Hub) GetUserPresence(userID string) store.UserPresence {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.effectivePresenceLocked(userID)
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		_ = c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Debug("ws.read_error", "err", err)
			}
			break
		}

		var msg HubMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "presence.set":
			var req struct {
				Status     string `json:"status"`
				StatusText string `json:"status_text"`
			}
			if msg.Payload != nil {
				if b, err := json.Marshal(msg.Payload); err == nil {
					_ = json.Unmarshal(b, &req)
				}
			}
			c.hub.UpdateUserPresence(context.Background(), c.userID, req.Status, req.StatusText)
		case "room.join":
			if msg.ChatID != "" {
				chatID, err := uuid.Parse(msg.ChatID)
				if err == nil && c.st != nil {
					var groups []string
					var dept, role string
					if c.claims != nil {
						groups = c.claims.ADGroups
						dept = c.claims.Department
						role = c.claims.Role
					}
					// Авторизация подписки: проверяем право доступа к чату перед добавлением в комнату WS
					ok, _, aErr := c.st.CanAccessChat(context.Background(), chatID, c.userID, groups, dept, role)
					if aErr != nil || !ok {
						slog.Warn("ws.room_join_denied", "chat_id", msg.ChatID, "user_id", c.userID)
						continue
					}
				}

				roomKey := "chat:" + msg.ChatID
				c.roomsMu.Lock()
				c.rooms[roomKey] = true
				c.roomsMu.Unlock()

				c.hub.mu.Lock()
				if _, ok := c.hub.roomClients[roomKey]; !ok {
					c.hub.roomClients[roomKey] = make(map[*Client]bool)
				}
				c.hub.roomClients[roomKey][c] = true
				c.hub.mu.Unlock()
			}
		case "room.leave":
			if msg.ChatID != "" {
				roomKey := "chat:" + msg.ChatID
				c.roomsMu.Lock()
				delete(c.rooms, roomKey)
				c.roomsMu.Unlock()

				c.hub.mu.Lock()
				if rMap, ok := c.hub.roomClients[roomKey]; ok {
					delete(rMap, c)
				}
				c.hub.mu.Unlock()
			}
		case "chat.typing":
			if msg.ChatID != "" {
				chatID, err := uuid.Parse(msg.ChatID)
				if err == nil && c.st != nil {
					var groups []string
					var dept, role string
					if c.claims != nil {
						groups = c.claims.ADGroups
						dept = c.claims.Department
						role = c.claims.Role
					}
					ok, _, aErr := c.st.CanAccessChat(context.Background(), chatID, c.userID, groups, dept, role)
					if aErr != nil || !ok {
						continue
					}
				}
				roomKey := "chat:" + msg.ChatID
				c.hub.BroadcastToRoom(roomKey, HubMessage{
					Type:    "chat.typing",
					ChatID:  msg.ChatID,
					TopicID: msg.TopicID,
					Payload: map[string]any{"user_id": c.userID},
				})
			}
		case "call.invite", "call.answered", "call.hangup", "call.rejected":
			// Сигналинг состояния вызова
			if msg.TargetID != "" {
				c.hub.SendDirect(msg.TargetID, HubMessage{
					Type:     msg.Type,
					ChatID:   msg.ChatID,
					TargetID: c.userID,
					Payload:  msg.Payload,
				})
			} else if msg.ChatID != "" {
				c.hub.BroadcastToRoom("chat:"+msg.ChatID, HubMessage{
					Type:     msg.Type,
					ChatID:   msg.ChatID,
					TargetID: c.userID,
					Payload:  msg.Payload,
				})
			}
		case "webrtc.signal":
			// Сигналинг WebRTC (SDP offer/answer, ICE candidate, ringing, hangup, reject)
			if msg.TargetID != "" {
				sigType, _ := msg.Payload["type"].(string)

				// Если звонящий сбросил или отменил вызов
				if sigType == "hangup" || sigType == "reject" || sigType == "cancel" {
					c.hub.CancelPendingCall(c.userID, msg.TargetID)
				}

				// Если это исходящий вызов (offer), а адресат прямо сейчас не в WebSocket
				if sigType == "offer" || sigType == "invite" {
					if !c.hub.IsUserOnline(msg.TargetID) {
						p := c.hub.GetUserPresence(msg.TargetID)
						if p.Status == "vacation" {
							statusLabel := "Абонент в отпуске"
							if p.StatusText != "" {
								statusLabel += " (" + p.StatusText + ")"
							}
							c.sendDirect(HubMessage{
								Type:     "webrtc.signal",
								ChatID:   msg.ChatID,
								TargetID: msg.TargetID,
								Payload: map[string]any{
									"type":   "offline",
									"status": statusLabel,
									"reason": "user_vacation",
								},
							})
							continue
						}

						// Ставим звонок в буфер ожидания пробуждения (35 секунд звонкового окна, как в TG)
						targetID := msg.TargetID
						chatID := msg.ChatID
						topicID := msg.TopicID

						isVideo := false
						if msg.Payload != nil {
							if iv, ok := msg.Payload["is_video"].(bool); ok {
								isVideo = iv
							}
						}
						go c.hub.notifyCoreCall(c.userID, targetID, chatID, isVideo)

						c.hub.CreatePendingCall(c.userID, targetID, chatID, topicID, msg.Payload, 35*time.Second, func() {
							// По истечении 35 секунд без ответа сообщаем вызывающему абоненту
							c.sendDirect(HubMessage{
								Type:     "webrtc.signal",
								ChatID:   chatID,
								TopicID:  topicID,
								TargetID: targetID,
								Payload: map[string]any{
									"type":   "offline",
									"status": "Абонент не ответил (таймаут)",
									"reason": "call_timeout",
								},
							})
						})

						// Отправляем звонящему статус ringing (звучат длинные гудки)
						c.sendDirect(HubMessage{
							Type:     "webrtc.signal",
							ChatID:   msg.ChatID,
							TargetID: msg.TargetID,
							Payload: map[string]any{
								"type":   "ringing",
								"status": "Вызов абонента...",
							},
						})
						continue
					}
				}

				c.hub.SendDirect(msg.TargetID, HubMessage{
					Type:     "webrtc.signal",
					ChatID:   msg.ChatID,
					TargetID: c.userID, // Отправитель становится target для ответа
					Payload:  msg.Payload,
				})
			}
		}
	}
}

func (c *Client) sendDirect(msg HubMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	select {
	case c.send <- data:
	default:
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			_, _ = w.Write(message)

			// Если в очереди есть еще сообщения, сбрасываем в тот же фрейм
			n := len(c.send)
			for i := 0; i < n; i++ {
				_, _ = w.Write([]byte{'\n'})
				_, _ = w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleWS — GET /api/v1/ws (апгрейд до WebSocket)
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var lastMsgID int64
	if sID := r.URL.Query().Get("last_message_id"); sID != "" {
		if val, err := strconv.ParseInt(sID, 10, 64); err == nil && val > 0 {
			lastMsgID = val
		}
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("ws.upgrade_failed", "err", err)
		return
	}

	userID := getUserID(claims)
	client := &Client{
		hub:    s.hub,
		st:     s.st,
		claims: claims,
		conn:   conn,
		userID: userID,
		send:   make(chan []byte, 256),
		rooms:  make(map[string]bool),
	}

	s.hub.register <- client

	// Сразу отправляем текущий снимок присутствия подключившемуся клиенту
	snapshot := s.hub.GetPresencesSnapshot()
	if b, err := json.Marshal(HubMessage{
		Type: "presence.snapshot",
		Payload: map[string]any{
			"presences": snapshot,
		},
	}); err == nil {
		select {
		case client.send <- b:
		default:
		}
	}

	// Если передан last_message_id, досылаем пропущенные клиентом сообщения
	if lastMsgID > 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			missed, err := s.st.ListMissedMessagesForUser(ctx, userID, claims.ADGroups, claims.Department, claims.Role, lastMsgID, 200)
			if err != nil {
				slog.Error("ws.gap_sync_failed", "user_id", userID, "since_id", lastMsgID, "err", err)
				return
			}
			for _, m := range missed {
				b, err := json.Marshal(map[string]any{
					"type": "chat.message",
					"payload": map[string]any{
						"message":  m,
						"gap_sync": true,
					},
				})
				if err != nil {
					continue
				}
				select {
				case client.send <- b:
				case <-time.After(500 * time.Millisecond):
					return
				}
			}
		}()
	}

	go client.writePump()
	go client.readPump()
}
