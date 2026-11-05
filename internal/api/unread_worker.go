package api

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type unreadUserState struct {
	lastNotifiedAt time.Time
	lastMaxMsgID   int64
	notifyCount    int
}

// UnreadReminderWorker отвечает за периодическую проверку непрочитанных сообщений
// и отправку push-напоминаний в мобильные и десктопные клиенты через Ligament 2FA Core.
type UnreadReminderWorker struct {
	srv      *Server
	interval time.Duration // Периодичность проверки и кулдаун напоминаний (по умолчанию 15 минут)
	minAge   time.Duration // Минимальное время с момента отправки сообщения до первого напоминания (по умолчанию 3 минуты)

	mu     sync.Mutex
	states map[string]*unreadUserState // userID -> state
}

// NewUnreadReminderWorker создаёт новый воркер напоминаний о непрочитанных сообщениях.
func NewUnreadReminderWorker(srv *Server, interval, minAge time.Duration) *UnreadReminderWorker {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	if minAge <= 0 {
		minAge = 3 * time.Minute
	}
	return &UnreadReminderWorker{
		srv:      srv,
		interval: interval,
		minAge:   minAge,
		states:   make(map[string]*unreadUserState),
	}
}

// Run запускает бесконечный цикл фоновой проверки с тикером.
func (w *UnreadReminderWorker) Run(ctx context.Context) {
	checkInterval := 1 * time.Minute
	if w.interval < checkInterval {
		checkInterval = w.interval / 2
		if checkInterval < 100*time.Millisecond {
			checkInterval = 100 * time.Millisecond
		}
	}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	slog.Info("chat: unread reminder worker started", "interval", w.interval, "min_age", w.minAge)

	for {
		select {
		case <-ctx.Done():
			slog.Info("chat: unread reminder worker stopped")
			return
		case <-ticker.C:
			w.checkAndNotify(ctx)
		}
	}
}

// checkAndNotify выполняет одну итерацию проверки непрочитанных сообщений.
func (w *UnreadReminderWorker) checkAndNotify(ctx context.Context) {
	if w.srv == nil || w.srv.st == nil || w.srv.hub == nil {
		return
	}

	summaries, err := w.srv.st.GetUsersWithUnreadMessages(ctx)
	if err != nil {
		slog.Warn("chat: failed to query unread summaries", "err", err)
		return
	}

	now := time.Now()
	activeUserIDs := make(map[string]bool)

	for _, u := range summaries {
		activeUserIDs[u.UserID] = true

		// 1. Подавление: если пользователь прямо сейчас в сети в чате через WebSocket
		if w.srv.hub.IsUserOnline(u.UserID) {
			continue
		}

		// 2. Порог минимального возраста: даём пользователю время прочитать сообщение без ложных пушей
		if now.Sub(u.LastMessageAt) < w.minAge {
			continue
		}

		w.mu.Lock()
		st, exists := w.states[u.UserID]
		shouldSend := false
		newCount := 1

		if !exists {
			shouldSend = true
		} else {
			// Проверяем кулдаун
			if now.Sub(st.lastNotifiedAt) >= w.interval {
				if u.MaxMessageID > st.lastMaxMsgID {
					// Пришли новые сообщения
					shouldSend = true
					newCount = 1
				} else if st.notifyCount < 3 {
					// Сообщения те же, но кулдаун прошёл и лимит 3 напоминаний не исчерпан
					shouldSend = true
					newCount = st.notifyCount + 1
				}
			}
		}

		if shouldSend {
			w.states[u.UserID] = &unreadUserState{
				lastNotifiedAt: now,
				lastMaxMsgID:   u.MaxMessageID,
				notifyCount:    newCount,
			}
		}
		w.mu.Unlock()

		if shouldSend {
			slog.Info("chat: sending unread reminder push",
				"target_user", u.UserID,
				"total_unread", u.TotalUnread,
				"chats_count", u.ChatsCount,
				"chat_id", u.ChatID,
				"notify_count", newCount,
			)
			w.srv.hub.notifyCoreUnread(
				u.UserID,
				u.TotalUnread,
				u.ChatsCount,
				u.ChatID.String(),
				u.ChatTitle,
				u.SenderName,
				u.MessageBody,
			)
		}
	}

	// 3. Очищаем состояние для пользователей, у которых больше нет непрочитанных сообщений
	w.mu.Lock()
	for uid := range w.states {
		if !activeUserIDs[uid] {
			delete(w.states, uid)
		}
	}
	w.mu.Unlock()
}
