package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/aligorov/ligament-chat/internal/store"
	"github.com/google/uuid"
)

// handleUploadAttachment — POST /api/v1/attachments/upload
func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	userID := getUserID(claims)
	if err := r.ParseMultipartForm(50 << 20); err != nil { // 50MB max
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_too_large_or_invalid_form"})
		return
	}

	chatIDStr := r.FormValue("chat_id")
	chatID, err := uuid.Parse(chatIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_chat_id"})
		return
	}

	ok, role, err := s.st.CanAccessChat(r.Context(), chatID, userID, claims.ADGroups, claims.Department, claims.Role)
	if err != nil || !ok || role == "viewer" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "upload_not_allowed"})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_file_field"})
		return
	}
	defer file.Close()

	attID := uuid.New()
	ext := filepath.Ext(header.Filename)
	saveDir := filepath.Join(s.dataDir, "attachments", chatID.String())
	if err := os.MkdirAll(saveDir, 0755); err != nil {
		slog.Error("chat: attachments mkdir failed", "err", err, "dir", saveDir)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_create_storage_dir"})
		return
	}

	storagePath := filepath.Join(saveDir, attID.String()+ext)
	dst, err := os.Create(storagePath)
	if err != nil {
		slog.Error("chat: attachments file create failed", "err", err, "path", storagePath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_save_file"})
		return
	}
	defer dst.Close()

	hasher := sha256.New()
	writer := io.MultiWriter(dst, hasher)
	size, err := io.Copy(writer, file)
	if err != nil {
		slog.Error("chat: attachments write file failed", "err", err, "path", storagePath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_write_file"})
		return
	}

	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	att := &store.Attachment{
		ID:          attID,
		ChatID:      chatID,
		FileName:    header.Filename,
		FileSize:    size,
		ContentType: contentType,
		StoragePath: storagePath,
		SHA256Hex:   hex.EncodeToString(hasher.Sum(nil)),
	}

	if err := s.st.SaveAttachment(r.Context(), att); err != nil {
		slog.Error("chat: attachments save meta failed", "err", err, "att_id", attID, "chat_id", chatID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed_to_save_attachment_meta"})
		return
	}

	writeJSON(w, http.StatusCreated, att)
}

// handleDownloadAttachment — GET /api/v1/attachments/{id}/download
func (s *Server) handleDownloadAttachment(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	attIDStr := r.PathValue("id")
	attID, err := uuid.Parse(attIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_attachment_id"})
		return
	}

	att, err := s.st.GetAttachment(r.Context(), attID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "attachment_not_found"})
		return
	}

	userID := getUserID(claims)
	ok, _, err := s.st.CanAccessChat(r.Context(), att.ChatID, userID, claims.ADGroups, claims.Department, claims.Role)
	if err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access_denied"})
		return
	}

	f, err := os.Open(att.StoragePath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file_not_found_on_disk"})
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if strings.HasPrefix(att.ContentType, "image/") || strings.HasPrefix(att.ContentType, "video/") || att.ContentType == "application/pdf" {
		w.Header().Set("Content-Disposition", "inline; filename=\""+att.FileName+"\"")
	} else {
		w.Header().Set("Content-Disposition", "attachment; filename=\""+att.FileName+"\"")
	}

	_, _ = io.Copy(w, f)
}
