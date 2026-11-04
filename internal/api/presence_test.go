package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aligorov/ligament-chat/internal/contract"
)

func TestPresenceHubLogic(t *testing.T) {
	hub := NewHub()
	stop := make(chan struct{})
	go hub.Run(stop)
	defer close(stop)

	// Пользователь еще не в сети и без статуса
	p0 := hub.GetUserPresence("user1")
	if p0.IsOnline {
		t.Fatalf("expected user1 to be offline, got online")
	}
	if p0.Status != "offline" {
		t.Fatalf("expected status 'offline', got %s", p0.Status)
	}

	// Установка статуса "в отпуске"
	pVac := hub.UpdateUserPresence(context.Background(), "user1", "vacation", "до 15 ноября")
	if pVac.Status != "vacation" {
		t.Fatalf("expected status 'vacation', got %s", pVac.Status)
	}
	if pVac.IsOnline {
		t.Fatalf("expected user1 to still be offline while not connected to ws")
	}

	// Проверка снимка
	snap := hub.GetPresencesSnapshot()
	if len(snap) == 0 {
		t.Fatalf("expected at least 1 user in snapshot, got 0")
	}
	found := false
	for _, p := range snap {
		if p.UserID == "user1" {
			found = true
			if p.Status != "vacation" || p.StatusText != "до 15 ноября" {
				t.Fatalf("snapshot mismatch: %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("user1 not found in snapshot")
	}

	// Установка статуса "занят"
	pBusy := hub.UpdateUserPresence(context.Background(), "user2", "busy", "на совещании")
	if pBusy.Status != "busy" {
		t.Fatalf("expected status 'busy', got %s", pBusy.Status)
	}
}

func TestPresenceAPIHandlers(t *testing.T) {
	hub := NewHub()
	srv := &Server{
		hub: hub,
	}

	// PUT /api/v1/presence
	body, _ := json.Marshal(map[string]any{
		"status":      "away",
		"status_text": "буду через 10 минут",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/presence", bytes.NewReader(body))
	// Добавляем claims
	claims := &contract.ChatClaims{
		Username: "employee1",
		Role:     "chat_user",
	}
	ctx := context.WithValue(req.Context(), ctxKeyClaims, claims)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.handleUpdatePresence(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var res map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if res["status"] != "away" || res["status_text"] != "буду через 10 минут" {
		t.Fatalf("unexpected response: %+v", res)
	}

	// GET /api/v1/presence
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/presence", nil)
	getReq = getReq.WithContext(ctx)
	getRec := httptest.NewRecorder()
	srv.handleGetPresence(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", getRec.Code, getRec.Body.String())
	}

	var getRes struct {
		Presences []map[string]any `json:"presences"`
	}
	if err := json.NewDecoder(getRec.Body).Decode(&getRes); err != nil {
		t.Fatalf("failed to decode get response: %v", err)
	}
	if len(getRes.Presences) == 0 {
		t.Fatalf("expected non-empty presences")
	}
}
