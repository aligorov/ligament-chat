package contract

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const AudienceChat = "ligament-chat"

// ChatClaims описывает JWT полезную нагрузку Ed25519 токена доступа к Ligament Chat.
type ChatClaims struct {
	Issuer      string   `json:"iss"`
	Audience    string   `json:"aud"`
	Subject     string   `json:"sub"`
	ID          string   `json:"jti"`
	IssuedAt    int64    `json:"iat"`
	NotBefore   int64    `json:"nbf"`
	ExpiresAt   int64    `json:"exp"`
	TenantScope string   `json:"tenant_scope"`
	Role        string   `json:"role"` // chat_admin, chat_moderator, chat_user, chat_viewer
	Username    string   `json:"preferred_username,omitempty"`
	ActorName   string   `json:"actor_name,omitempty"`
	Department  string   `json:"department,omitempty"`
	ChatGroups  []string `json:"chat_groups,omitempty"`
	ADGroups    []string `json:"ad_groups,omitempty"`
}

// SignChatToken подписывает Claims приватным ключом Ed25519.
func SignChatToken(claims ChatClaims, privKey ed25519.PrivateKey) (string, error) {
	if len(privKey) != ed25519.PrivateKeySize {
		return "", errors.New("contract: invalid private key size")
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString(raw)
	message := header + "." + payload
	sig := ed25519.Sign(privKey, []byte(message))
	return message + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyChatToken проверяет Ed25519 подпись и валидность токена доступа к чату.
func VerifyChatToken(tokenStr string, keyLookup func(issuer string) (ed25519.PublicKey, error)) (*ChatClaims, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, errors.New("contract: malformed token parts")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("contract: invalid header base64: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("contract: unmarshal header: %w", err)
	}
	if header.Alg != "EdDSA" {
		return nil, fmt.Errorf("contract: unsupported algorithm: %s", header.Alg)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("contract: invalid payload base64: %w", err)
	}
	var claims ChatClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("contract: unmarshal claims: %w", err)
	}

	if claims.Audience != AudienceChat {
		return nil, fmt.Errorf("contract: audience mismatch %q != %q", claims.Audience, AudienceChat)
	}

	now := time.Now().UTC().Unix()
	if claims.ExpiresAt < now-30 {
		return nil, errors.New("contract: token expired")
	}
	if claims.NotBefore > now+30 {
		return nil, errors.New("contract: token not yet valid")
	}

	pubKey, err := keyLookup(claims.Issuer)
	if err != nil {
		return nil, fmt.Errorf("contract: issuer key lookup failed: %w", err)
	}
	if len(pubKey) != ed25519.PublicKeySize {
		return nil, errors.New("contract: invalid issuer public key size")
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("contract: invalid signature base64: %w", err)
	}

	signedContent := parts[0] + "." + parts[1]
	if !ed25519.Verify(pubKey, []byte(signedContent), sigBytes) {
		return nil, errors.New("contract: invalid ed25519 signature")
	}

	return &claims, nil
}
