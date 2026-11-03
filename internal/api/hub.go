package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
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
	mu          sync.RWMutex
	clients     map[*Client]bool
	userClients map[string]map[*Client]bool
	roomClients map[string]map[*Client]bool
	register    chan *Client
	unregister  chan *Client
	broadcast   chan *broadcastEvent
	direct      chan *directEvent
}

type broadcastEvent struct {
	room string
	data []byte
}

type directEvent struct {
	targetUserID string
	data         []byte
}

func NewHub() *Hub {
	return &Hub{
		clients:     make(map[*Client]bool),
		userClients: make(map[string]map[*Client]bool),
		roomClients: make(map[string]map[*Client]bool),
		register:    make(chan *Client),
		unregister:  make(chan *Client),
		broadcast:   make(chan *broadcastEvent, 256),
		direct:      make(chan *directEvent, 256),
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
			h.userClients[client.userID][client] = true
			h.mu.Unlock()

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				if uMap, ok := h.userClients[client.userID]; ok {
					delete(uMap, client)
					if len(uMap) == 0 {
						delete(h.userClients, client.userID)
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
			h.mu.Unlock()

		case ev := <-h.broadcast:
			h.mu.RLock()
			for client := range h.roomClients[ev.room] {
				select {
				case client.send <- ev.data:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
			h.mu.RUnlock()

		case ev := <-h.direct:
			h.mu.RLock()
			for client := range h.userClients[ev.targetUserID] {
				select {
				case client.send <- ev.data:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
			h.mu.RUnlock()
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
				roomKey := "chat:" + msg.ChatID
				c.hub.BroadcastToRoom(roomKey, HubMessage{
					Type:    "chat.typing",
					ChatID:  msg.ChatID,
					TopicID: msg.TopicID,
					Payload: map[string]any{"user_id": c.userID},
				})
			}
		case "webrtc.signal":
			// Сигналинг WebRTC (SDP offer/answer, ICE candidate)
			if msg.TargetID != "" {
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

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("ws.upgrade_failed", "err", err)
		return
	}

	client := &Client{
		hub:     s.hub,
		st:      s.st,
		claims:  claims,
		conn:    conn,
		userID:  claims.Subject,
		send:    make(chan []byte, 256),
		rooms:   make(map[string]bool),
	}

	s.hub.register <- client

	go client.writePump()
	go client.readPump()
}
