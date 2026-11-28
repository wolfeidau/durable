package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// parsePrivateKey parses a PKCS#1 or PKCS#8 RSA PEM private key without echoing key material in errors.
func parsePrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("GitHub App private key is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("GitHub App private key must be an RSA PKCS#1 or PKCS#8 PEM key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("GitHub App private key must be an RSA key")
	}
	return key, nil
}

// signAppJWT signs a short-lived RS256 GitHub App JWT with clientID as issuer.
func signAppJWT(clientID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	if strings.TrimSpace(clientID) == "" {
		return "", errors.New("GitHub App client_id is required")
	}
	if key == nil {
		return "", errors.New("GitHub App private key is required")
	}
	header, err := json.Marshal(struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}{Alg: "RS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("failed to encode JWT header: %w", err)
	}
	claims, err := json.Marshal(struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{
		Iss: clientID,
		Iat: now.Add(-60 * time.Second).Unix(),
		Exp: now.Add(5 * time.Minute).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("failed to encode JWT claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("failed to sign GitHub App JWT: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
