package api

import (
	"testing"

	"github.com/aligorov/ligament-chat/internal/store"
)

func TestIsChatManager(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		globalRole string
		want       bool
	}{
		{"member regular", store.MemberRoleMember, "user", false},
		{"viewer regular", "viewer", "user", false},
		{"admin in chat", store.MemberRoleAdmin, "user", true},
		{"moderator in chat", store.MemberRoleModerator, "user", true},
		{"global admin", store.MemberRoleMember, "admin", true},
		{"global superadmin", store.MemberRoleMember, "superadmin", true},
		{"global chat_admin", store.MemberRoleMember, "chat_admin", true},
		{"global moderator", store.MemberRoleMember, "moderator", true},
		{"global operator", store.MemberRoleMember, "operator", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isChatManager(tc.role, tc.globalRole)
			if got != tc.want {
				t.Errorf("isChatManager(%q, %q) = %v; want %v", tc.role, tc.globalRole, got, tc.want)
			}
		})
	}
}
