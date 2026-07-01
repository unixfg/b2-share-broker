package broker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeAuth struct {
	principal Principal
	err       error
}

func (a fakeAuth) Authenticate(context.Context, string) (Principal, error) {
	if a.err != nil {
		return Principal{}, a.err
	}
	return a.principal, nil
}

type fakeStore struct {
	presign PresignedUpload
	head    ObjectMetadata
	headErr error
}

func (s fakeStore) PresignPutObject(context.Context, string, string, int64, time.Duration) (PresignedUpload, error) {
	return s.presign, nil
}

func (s fakeStore) HeadObject(context.Context, string) (ObjectMetadata, error) {
	if s.headErr != nil {
		return ObjectMetadata{}, s.headErr
	}
	return s.head, nil
}

func testConfig() Config {
	return Config{
		B2PublicBaseURL: "https://bucket.s3.us-west-004.backblazeb2.com",
		PublicBaseURL:   "https://share.doesthings.online",
		ObjectPrefix:    "share-broker",
		MaxUploadBytes:  1024,
		PresignTTL:      15 * time.Minute,
		UploadTokenTTL:  time.Hour,
		UploadTokenKey:  []byte("01234567890123456789012345678901"),
		Clock:           func() time.Time { return time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC) },
		Entropy:         strings.NewReader("0123456789"),
	}
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, fakeStore{}, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder.Body.String() != "ok\n" {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}

func TestCreateUpload(t *testing.T) {
	server := NewServer(
		testConfig(),
		fakeAuth{principal: Principal{Subject: "user-1"}},
		fakeStore{presign: PresignedUpload{
			URL:    "https://upload.example.test/presigned",
			Header: http.Header{"Content-Type": []string{"image/png"}, "Host": []string{"ignored"}},
		}},
		slog.Default(),
	)
	body := `{"filename":"Screenshot 1.png","contentType":"image/png","size":42}`
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response createUploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.UploadURL != "https://upload.example.test/presigned" {
		t.Fatalf("upload URL = %q", response.UploadURL)
	}
	if response.RequiredHeaders["Content-Type"] != "image/png" {
		t.Fatalf("required headers = %#v", response.RequiredHeaders)
	}
	if !strings.Contains(response.ObjectKey, "/Screenshot_1.png") {
		t.Fatalf("object key = %q", response.ObjectKey)
	}
	if response.PublicURL == "" || response.UploadToken == "" {
		t.Fatalf("response missing public URL or token: %#v", response)
	}
	if !strings.HasPrefix(response.PublicURL, "https://share.doesthings.online/s/share-broker/") {
		t.Fatalf("public URL = %q", response.PublicURL)
	}
	if !strings.Contains(response.PublicURL, "/Screenshot_1.png") {
		t.Fatalf("public URL = %q", response.PublicURL)
	}
}

func TestCreateUploadRejectsUnauthenticated(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, fakeStore{}, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader(`{"filename":"a.txt","size":1}`))
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestCreateUploadRejectsOversizedFile(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{principal: Principal{Subject: "user-1"}}, fakeStore{}, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader(`{"filename":"a.bin","size":2048}`))
	request.Header.Set("Authorization", "Bearer test")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestCompleteUploadReturnsVerifiedMetadata(t *testing.T) {
	cfg := testConfig()
	token, err := SignUploadToken(cfg.UploadTokenKey, uploadTokenPayload{
		ObjectKey:   "share-broker/2026/06/28/01J00000000000000000000000/a.txt",
		ContentType: "text/plain",
		Size:        12,
		Subject:     "user-1",
		ExpiresAt:   cfg.Clock().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(
		cfg,
		fakeAuth{principal: Principal{Subject: "user-1"}},
		fakeStore{head: ObjectMetadata{ContentLength: 12, ETag: "abc123"}},
		slog.Default(),
	)
	request := httptest.NewRequest(http.MethodPost, "/api/uploads/complete", strings.NewReader(`{"uploadToken":"`+token+`"}`))
	request.Header.Set("Authorization", "Bearer test")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response completeUploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Verified || response.Size != 12 || response.ETag != "abc123" {
		t.Fatalf("response = %#v", response)
	}
	if response.PublicURL != "https://share.doesthings.online/s/share-broker/2026/06/28/01J00000000000000000000000/a.txt" {
		t.Fatalf("public URL = %q", response.PublicURL)
	}
}

func TestCompleteUploadAllowsBestEffortHeadFailure(t *testing.T) {
	cfg := testConfig()
	token, err := SignUploadToken(cfg.UploadTokenKey, uploadTokenPayload{
		ObjectKey: "share-broker/key.txt",
		Size:      12,
		Subject:   "user-1",
		ExpiresAt: cfg.Clock().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(
		cfg,
		fakeAuth{principal: Principal{Subject: "user-1"}},
		fakeStore{headErr: errors.New("transient")},
		slog.Default(),
	)
	request := httptest.NewRequest(http.MethodPost, "/api/uploads/complete", strings.NewReader(`{"uploadToken":"`+token+`"}`))
	request.Header.Set("Authorization", "Bearer test")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPublicShareRedirectsToNativeB2URL(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, fakeStore{}, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/s/share-broker/2026/06/28/01J00000000000000000000000/file%20name.txt", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	want := "https://bucket.s3.us-west-004.backblazeb2.com/share-broker/2026/06/28/01J00000000000000000000000/file%20name.txt"
	if got := recorder.Header().Get("Location"); got != want {
		t.Fatalf("location = %q, want %q", got, want)
	}
}

func TestPublicShareHeadRedirectsToNativeB2URL(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, fakeStore{}, slog.Default())
	request := httptest.NewRequest(http.MethodHead, "/s/share-broker/key.txt", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Location"); got != "https://bucket.s3.us-west-004.backblazeb2.com/share-broker/key.txt" {
		t.Fatalf("location = %q", got)
	}
}

func TestPublicShareRejectsObjectsOutsideConfiguredPrefix(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, fakeStore{}, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/s/other-prefix/key.txt", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}
