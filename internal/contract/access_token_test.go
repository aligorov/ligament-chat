package contract

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestSignAndVerifyChatToken(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	issuer := "core-node-1"
	now := time.Now().UTC()
	claims := ChatClaims{
		Issuer:      issuer,
		Audience:    AudienceChat,
		Subject:     "usr-uuid-1234",
		Username:    "ivanov",
		ActorName:   "Иванов Иван",
		Role:        "chat_user",
		Department:  "Бухгалтерия",
		ChatGroups:  []string{"buh", "all"},
		ADGroups:    []string{"Domain Users", "Accountants"},
		IssuedAt:    now.Unix(),
		NotBefore:   now.Unix(),
		ExpiresAt:   now.Add(10 * time.Minute).Unix(),
		TenantScope: "default",
	}

	tokenStr, err := SignChatToken(claims, priv)
	if err != nil {
		t.Fatalf("SignChatToken failed: %v", err)
	}

	lookup := func(iss string) (ed25519.PublicKey, error) {
		if iss != issuer {
			t.Fatalf("unexpected issuer lookup: %s", iss)
		}
		return pub, nil
	}

	verified, err := VerifyChatToken(tokenStr, lookup)
	if err != nil {
		t.Fatalf("VerifyChatToken failed: %v", err)
	}

	if verified.Subject != "usr-uuid-1234" {
		t.Errorf("subject mismatch: got %s, want %s", verified.Subject, "usr-uuid-1234")
	}
	if verified.Username != "ivanov" {
		t.Errorf("username mismatch: got %s, want %s", verified.Username, "ivanov")
	}
	if verified.Role != "chat_user" {
		t.Errorf("role mismatch: got %s, want %s", verified.Role, "chat_user")
	}
	if len(verified.ChatGroups) != 2 || verified.ChatGroups[0] != "buh" {
		t.Errorf("chat_groups mismatch: %v", verified.ChatGroups)
	}
}

func TestVerifyChatTokenErrors(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)

	now := time.Now().UTC()
	claims := ChatClaims{
		Issuer:    "core-node-1",
		Audience:  AudienceChat,
		Subject:   "usr-1",
		IssuedAt:  now.Unix(),
		NotBefore: now.Unix(),
		ExpiresAt: now.Add(5 * time.Minute).Unix(),
	}

	tokenStr, _ := SignChatToken(claims, priv)

	// Case 1: Wrong public key
	_, err := VerifyChatToken(tokenStr, func(string) (ed25519.PublicKey, error) {
		return otherPub, nil
	})
	if err == nil {
		t.Error("expected error for wrong public key, got nil")
	}

	// Case 2: Expired token
	expiredClaims := claims
	expiredClaims.ExpiresAt = now.Add(-1 * time.Hour).Unix()
	expiredToken, _ := SignChatToken(expiredClaims, priv)
	_, err = VerifyChatToken(expiredToken, func(string) (ed25519.PublicKey, error) {
		return otherPub, nil
	})
	if err == nil {
		t.Error("expected error for expired token, got nil")
	}

	// Case 3: Wrong audience
	wrongAudClaims := claims
	wrongAudClaims.Audience = "wrong-aud"
	wrongAudToken, _ := SignChatToken(wrongAudClaims, priv)
	_, err = VerifyChatToken(wrongAudToken, func(string) (ed25519.PublicKey, error) {
		return otherPub, nil
	})
	if err == nil {
		t.Error("expected error for wrong audience, got nil")
	}
}
