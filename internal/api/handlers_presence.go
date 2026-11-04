package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleGetPresence — GET /api/v1/presence
// Возвращает снимок всех сотрудников компании с их статусом и онлайн-состоянием в чате.
func (s *Server) handleGetPresence(w http.ResponseWriter, r *http.Request) {
	snapshot := s.hub.GetPresencesSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"presences": snapshot,
	})
}

type updatePresenceReq struct {
	Status     string `json:"status"`      // online, busy, vacation, away, dnd
	StatusText string `json:"status_text"` // опциональный комментарий к статусу
}

// handleUpdatePresence — PUT /api/v1/presence или POST /api/v1/presence
// Устанавливает статус присутствия текущего пользователя.
func (s *Server) handleUpdatePresence(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	userID := getUserID(claims)
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req updatePresenceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	req.Status = strings.TrimSpace(req.Status)
	if req.Status == "" {
		req.Status = "online"
	}

	p := s.hub.UpdateUserPresence(r.Context(), userID, req.Status, req.StatusText)
	writeJSON(w, http.StatusOK, p)
}
