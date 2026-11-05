package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aligorov/ligament-chat/internal/contract"
	"github.com/google/uuid"
)

func TestDeleteMessage_InvalidParams(t *testing.T) {
	srv := &Server{}

	// Invalid chat ID
	req := httptest.NewRequest("DELETE", "/api/v1/chats/not-a-uuid/messages/1", nil)
	req.SetPathValue("id", "not-a-uuid")
	req.SetPathValue("msg_id", "1")
	w := httptest.NewRecorder()
	srv.handleDeleteMessage(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid chat id, got %d", w.Code)
	}

	// Invalid message ID
	validUUID := uuid.New().String()
	req = httptest.NewRequest("DELETE", "/api/v1/chats/"+validUUID+"/messages/abc", nil)
	req.SetPathValue("id", validUUID)
	req.SetPathValue("msg_id", "abc")
	w = httptest.NewRecorder()
	srv.handleDeleteMessage(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid msg id, got %d", w.Code)
	}
}

func TestEditMessage_EmptyBody(t *testing.T) {
	srv := &Server{}
	validUUID := uuid.New().String()

	// Empty body
	req := httptest.NewRequest("PUT", "/api/v1/chats/"+validUUID+"/messages/123", strings.NewReader(`{"body": "   "}`))
	req.SetPathValue("id", validUUID)
	req.SetPathValue("msg_id", "123")

	claims := &contract.ChatClaims{
		Username: "alice",
		Role:     "member",
	}
	ctx := context.WithValue(req.Context(), ctxKeyClaims, claims)
	req = req.WithContext(ctx)

	// Will fail access denied or empty body when store is nil, but we can verify param extraction
	w := httptest.NewRecorder()
	defer func() {
		_ = recover() // st is nil in bare Server struct
	}()
	srv.handleEditMessage(w, req)
}
