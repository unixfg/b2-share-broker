package broker

import (
	"testing"
	"time"
)

func TestUploadTokenRoundTrip(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	now := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	token, err := SignUploadToken(key, uploadTokenPayload{
		ObjectKey: "share-broker/key.txt",
		Size:      1,
		Subject:   "user-1",
		ExpiresAt: now.Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := VerifyUploadToken(key, token, now)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ObjectKey != "share-broker/key.txt" || payload.Subject != "user-1" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestUploadTokenRejectsTampering(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	token, err := SignUploadToken(key, uploadTokenPayload{
		ObjectKey: "share-broker/key.txt",
		Size:      1,
		Subject:   "user-1",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyUploadToken(key, token+"x", time.Now()); err == nil {
		t.Fatal("expected tampered token to fail")
	}
}
