package api

import (
	"net/http"

	"github.com/aligorov/ligament-chat/internal/store"
)

type Server struct {
	st          *store.Store
	hub         *Hub
	mux         *http.ServeMux
	dataDir     string
	recorderURL string
	coreURL     string
}

func NewServer(st *store.Store, hub *Hub, dataDir, recorderURL, coreURL string) *Server {
	s := &Server{
		st:          st,
		hub:         hub,
		mux:         http.NewServeMux(),
		dataDir:     dataDir,
		recorderURL: recorderURL,
		coreURL:     coreURL,
	}

	s.routes()
	return s
}

func (s *Server) routes() {
	// Healthcheck
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"status":   "ok",
			"service":  "ligament-chat",
			"core_url": s.coreURL,
		})
	})

	// Core Trust handshake
	s.mux.HandleFunc("POST /api/v1/auth/trust", s.handleRegisterTrust)

	// WebSocket Gateway
	s.mux.Handle("GET /api/v1/ws", s.authMiddleware(http.HandlerFunc(s.handleWS)))

	// Chats & Topics
	s.mux.Handle("GET /api/v1/chats", s.authMiddleware(http.HandlerFunc(s.handleListChats)))
	s.mux.Handle("POST /api/v1/chats", s.authMiddleware(http.HandlerFunc(s.handleCreateChat)))
	s.mux.Handle("GET /api/v1/chats/{id}", s.authMiddleware(http.HandlerFunc(s.handleGetChat)))
	s.mux.Handle("GET /api/v1/chats/{id}/topics", s.authMiddleware(http.HandlerFunc(s.handleListTopics)))
	s.mux.Handle("POST /api/v1/chats/{id}/topics", s.authMiddleware(http.HandlerFunc(s.handleCreateTopic)))
	s.mux.Handle("GET /api/v1/chats/{id}/messages", s.authMiddleware(http.HandlerFunc(s.handleListMessages)))
	s.mux.Handle("POST /api/v1/chats/{id}/messages", s.authMiddleware(http.HandlerFunc(s.handleSendMessage)))
	s.mux.Handle("POST /api/v1/chats/{id}/read", s.authMiddleware(http.HandlerFunc(s.handleReadPointer)))

	// WebRTC Calls
	s.mux.Handle("POST /api/v1/calls/start", s.authMiddleware(http.HandlerFunc(s.handleStartCall)))
	s.mux.Handle("POST /api/v1/calls/{id}/status", s.authMiddleware(http.HandlerFunc(s.handleCallStatus)))
	s.mux.Handle("POST /api/v1/calls/{id}/record", s.authMiddleware(http.HandlerFunc(s.handleToggleRecording)))

	// Attachments
	s.mux.Handle("POST /api/v1/attachments/upload", s.authMiddleware(http.HandlerFunc(s.handleUploadAttachment)))
	s.mux.Handle("GET /api/v1/attachments/{id}/download", s.authMiddleware(http.HandlerFunc(s.handleDownloadAttachment)))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS headers for native apps / desktop
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Requested-With")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.mux.ServeHTTP(w, r)
}
