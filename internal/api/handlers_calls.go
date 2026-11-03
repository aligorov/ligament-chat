package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/aligorov/ligament-chat/internal/store"
	"github.com/google/uuid"
)

type startCallReq struct {
	ChatID       uuid.UUID `json:"chat_id"`
	CalleeUserID string    `json:"callee_user_id"`
}

// handleStartCall — POST /api/v1/calls/start
func (s *Server) handleStartCall(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	userID := getUserID(claims)
	var req startCallReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	if req.CalleeUserID == "" || req.ChatID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_required_fields"})
		return
	}

	callerName := claims.ActorName
	if callerName == "" {
		callerName = userID
	}

	call := &store.CallSession{
		ID:           uuid.New(),
		ChatID:       req.ChatID,
		CallerUserID: userID,
		CalleeUserID: req.CalleeUserID,
		Status:       "ringing",
	}

	if err := s.st.CreateCallSession(r.Context(), call); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_create_call"})
		return
	}

	// Отправляем прямой вызов собеседнику через WebSocket
	s.hub.SendDirect(req.CalleeUserID, HubMessage{
		Type:     "call.invite",
		ChatID:   req.ChatID.String(),
		TargetID: req.CalleeUserID,
		Payload: map[string]any{
			"call_id":        call.ID.String(),
			"caller_user_id": userID,
			"caller_name":    callerName,
		},
	})

	writeJSON(w, http.StatusCreated, call)
}

type updateCallStatusReq struct {
	Status string `json:"status"` // active, ended, rejected, missed
}

// handleCallStatus — POST /api/v1/calls/{id}/status
func (s *Server) handleCallStatus(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	userID := getUserID(claims)

	callIDStr := r.PathValue("id")
	callID, err := uuid.Parse(callIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_call_id"})
		return
	}

	call, err := s.st.GetCallSession(r.Context(), callID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "call_not_found"})
		return
	}

	isAdmin := claims != nil && (claims.Role == "admin" || claims.Role == "superadmin" || claims.Role == "chat_admin")
	if !isAdmin && call.CallerUserID != userID && call.CalleeUserID != userID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access_denied_to_call"})
		return
	}

	var req updateCallStatusReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	if err := s.st.UpdateCallSession(r.Context(), callID, req.Status, false, nil); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_update_call"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type toggleRecordingReq struct {
	Record bool `json:"record"`
}

// handleToggleRecording — POST /api/v1/calls/{id}/record
// Переключение записи звонка в ligament-recoder
func (s *Server) handleToggleRecording(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	userID := getUserID(claims)

	callIDStr := r.PathValue("id")
	callID, err := uuid.Parse(callIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_call_id"})
		return
	}

	call, err := s.st.GetCallSession(r.Context(), callID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "call_not_found"})
		return
	}

	isAdmin := claims != nil && (claims.Role == "admin" || claims.Role == "superadmin" || claims.Role == "chat_admin")
	if !isAdmin && call.CallerUserID != userID && call.CalleeUserID != userID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access_denied_to_call"})
		return
	}

	var req toggleRecordingReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	var recSessionID *uuid.UUID
	if req.Record {
		genID := uuid.New()
		recSessionID = &genID

		// Если рекордер настроен, уведомляем Ingest API
		if s.recorderURL != "" {
			go s.notifyRecorderCallStart(context.Background(), *recSessionID, callID)
		}
	}

	if err := s.st.UpdateCallSession(r.Context(), callID, "active", req.Record, recSessionID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_update_recording_state"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"is_recorded":         req.Record,
		"recorder_session_id": recSessionID,
	})
}

func (s *Server) notifyRecorderCallStart(ctx context.Context, recID, callID uuid.UUID) {
	body, _ := json.Marshal(map[string]any{
		"session_id":   recID.String(),
		"session_kind": "call",
		"actor_name":   "Ligament Call",
		"metadata": map[string]any{
			"call_id": callID.String(),
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.recorderURL+"/api/v1/ingest/sessions/start", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}
