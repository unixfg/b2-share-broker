package broker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type uploadTokenPayload struct {
	ObjectKey       string `json:"objectKey"`
	SHA256          string `json:"sha256"`
	AliasSlug       string `json:"aliasSlug"`
	DisplayFilename string `json:"displayFilename"`
	ContentType     string `json:"contentType"`
	Extension       string `json:"extension"`
	Size            int64  `json:"size"`
	Subject         string `json:"sub"`
	ExpiresAt       int64  `json:"exp"`
}

func SignUploadToken(key []byte, payload uploadTokenPayload) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	bodyEncoded := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(bodyEncoded))
	signature := mac.Sum(nil)
	return bodyEncoded + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func VerifyUploadToken(key []byte, token string, now time.Time) (uploadTokenPayload, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return uploadTokenPayload{}, errors.New("invalid upload token")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return uploadTokenPayload{}, errors.New("invalid upload token signature")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return uploadTokenPayload{}, errors.New("invalid upload token signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return uploadTokenPayload{}, errors.New("invalid upload token payload")
	}
	var payload uploadTokenPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return uploadTokenPayload{}, errors.New("invalid upload token payload")
	}
	if payload.ObjectKey == "" || payload.SHA256 == "" || payload.AliasSlug == "" || payload.Subject == "" || payload.Size <= 0 {
		return uploadTokenPayload{}, errors.New("invalid upload token payload")
	}
	if now.Unix() > payload.ExpiresAt {
		return uploadTokenPayload{}, fmt.Errorf("upload token expired")
	}
	return payload, nil
}
