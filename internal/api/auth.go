package api

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aligorov/ligament-chat/internal/contract"
	"github.com/google/uuid"
)

type ctxKeyType struct{}

var ctxKeyClaims = ctxKeyType{}

func claimsFromContext(ctx context.Context) (*contract.ChatClaims, bool) {
	c, ok := ctx.Value(ctxKeyClaims).(*contract.ChatClaims)
	return c, ok && c != nil
}

type registerTrustRequest struct {
	InstanceID   string `json:"instance_id"`
	PublicKeyHex string `json:"public_key_hex"`
	TenantScope  string `json:"tenant_scope"`
	Timestamp    int64  `json:"timestamp"`
	Signature    string `json:"signature"`
}

// handleRegisterTrust — POST /api/v1/auth/trust
// Регистрация Ed25519 публичного ключа инстанса Core для верификации SSO-билетов.
func (s *Server) handleRegisterTrust(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req registerTrustRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request_body"})
		return
	}

	instanceID, err := uuid.Parse(req.InstanceID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_instance_id"})
		return
	}

	now := time.Now().UTC().Unix()
	if req.Timestamp < now-300 || req.Timestamp > now+300 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expired_timestamp"})
		return
	}

	pubBytes, err := hex.DecodeString(req.PublicKeyHex)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_public_key"})
		return
	}

	sigBytes, err := hex.DecodeString(req.Signature)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_signature_format"})
		return
	}

	if req.TenantScope == "" {
		req.TenantScope = "default"
	}

	msg := fmt.Sprintf("ligament-chat-trust:%s:%s:%s:%d", instanceID.String(), req.PublicKeyHex, req.TenantScope, req.Timestamp)
	if !ed25519.Verify(pubBytes, []byte(msg), sigBytes) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid_signature"})
		return
	}

	if err := s.st.SaveTrust(r.Context(), instanceID.String(), req.PublicKeyHex, req.TenantScope); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_save_trust"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"instance_id": instanceID.String(),
	})
}

// authMiddleware проверяет Bearer-токен или query-параметр ?token= (для WebSocket)
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenStr := ""
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			tokenStr = strings.TrimPrefix(authHeader, "Bearer ")
		} else if qToken := r.URL.Query().Get("token"); qToken != "" {
			tokenStr = qToken
		}

		if tokenStr == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing_token"})
			return
		}

		keyLookup := func(issuer string) (ed25519.PublicKey, error) {
			pub, _, err := s.st.GetTrust(r.Context(), issuer)
			if err != nil {
				return nil, errors.New("untrusted_issuer")
			}
			return pub, nil
		}

		claims, err := contract.VerifyChatToken(tokenStr, keyLookup)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token: " + err.Error()})
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeyClaims, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
