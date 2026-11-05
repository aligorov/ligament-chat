package api

import (
	"context"
	"testing"
	"time"
)

func TestUnreadReminderWorker_Defaults(t *testing.T) {
	w := NewUnreadReminderWorker(nil, 0, 0)
	if w.interval != 15*time.Minute {
		t.Fatalf("expected default interval 15m, got %v", w.interval)
	}
	if w.minAge != 3*time.Minute {
		t.Fatalf("expected default minAge 3m, got %v", w.minAge)
	}
}

func TestUnreadReminderWorker_CheckAndNotify_NilSafe(t *testing.T) {
	w := NewUnreadReminderWorker(nil, 10*time.Minute, 2*time.Minute)
	// Should not panic with nil srv
	w.checkAndNotify(context.Background())
}

func TestUnreadReminderWorker_StateTracking(t *testing.T) {
	w := NewUnreadReminderWorker(&Server{}, 100*time.Millisecond, 50*time.Millisecond)

	w.states["user1"] = &unreadUserState{
		lastNotifiedAt: time.Now().Add(-200 * time.Millisecond),
		lastMaxMsgID:   10,
		notifyCount:    1,
	}

	w.mu.Lock()
	st, exists := w.states["user1"]
	w.mu.Unlock()

	if !exists || st.notifyCount != 1 || st.lastMaxMsgID != 10 {
		t.Fatalf("unexpected state: %+v", st)
	}
}
