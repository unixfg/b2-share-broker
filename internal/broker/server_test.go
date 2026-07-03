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

const testSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeAuth struct {
	authenticated AuthenticatedRequest
	err           error
}

func (a fakeAuth) Authenticate(*http.Request) (AuthenticatedRequest, error) {
	if a.err != nil {
		return AuthenticatedRequest{}, a.err
	}
	return a.authenticated, nil
}

type fakeStore struct {
	presign       PresignedUpload
	head          ObjectMetadata
	headErr       error
	presignedKey  string
	presignedType string
}

func (s *fakeStore) PresignPutObject(_ context.Context, key, contentType string, _ int64, _ time.Duration) (PresignedUpload, error) {
	s.presignedKey = key
	s.presignedType = contentType
	return s.presign, nil
}

func (s *fakeStore) HeadObject(context.Context, string) (ObjectMetadata, error) {
	if s.headErr != nil {
		return ObjectMetadata{}, s.headErr
	}
	return s.head, nil
}

type memoryMetadata struct {
	objects map[string]StoredObject
	aliases map[string]ShareAlias
	history []ShareAlias
}

func newMemoryMetadata() *memoryMetadata {
	return &memoryMetadata{
		objects: map[string]StoredObject{},
		aliases: map[string]ShareAlias{},
	}
}

func (m *memoryMetadata) GetObject(_ context.Context, sha256 string) (StoredObject, bool, error) {
	object, ok := m.objects[sha256]
	return object, ok, nil
}

func (m *memoryMetadata) UpsertObjectAndAlias(_ context.Context, object StoredObject, alias ShareAlias) error {
	if object.CreatedAt.IsZero() {
		object.CreatedAt = time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	}
	m.objects[object.SHA256] = object
	return m.UpsertAlias(context.Background(), alias)
}

func (m *memoryMetadata) UpsertAlias(_ context.Context, alias ShareAlias) error {
	if previous, ok := m.aliases[alias.Slug]; ok && previous.ObjectSHA256 != alias.ObjectSHA256 {
		m.history = append(m.history, previous)
	}
	object := m.objects[alias.ObjectSHA256]
	alias.ObjectKey = object.ObjectKey
	alias.Size = object.Size
	alias.ContentType = object.ContentType
	if alias.CreatedAt.IsZero() {
		alias.CreatedAt = time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	}
	alias.UpdatedAt = alias.CreatedAt
	m.aliases[alias.Slug] = alias
	return nil
}

func (m *memoryMetadata) GetAlias(_ context.Context, slug string) (ShareAlias, bool, error) {
	alias, ok := m.aliases[slug]
	return alias, ok, nil
}

func (m *memoryMetadata) RecordAliasRedirect(_ context.Context, slug string) error {
	alias, ok := m.aliases[slug]
	if !ok {
		return errors.New("alias not found")
	}
	alias.RedirectCount++
	alias.LastRedirectedAt = time.Date(2026, 6, 28, 12, 1, 0, 0, time.UTC)
	m.aliases[slug] = alias
	return nil
}

func (m *memoryMetadata) ListAliases(_ context.Context, owner string, _ int) ([]ShareAlias, error) {
	var aliases []ShareAlias
	for _, alias := range m.aliases {
		if alias.Owner == owner && alias.Visibility != "deleted" {
			aliases = append(aliases, alias)
		}
	}
	return aliases, nil
}

func (m *memoryMetadata) DeleteAlias(_ context.Context, slug, owner string) (bool, error) {
	alias, ok := m.aliases[slug]
	if !ok || alias.Owner != owner || alias.Visibility == "deleted" {
		return false, nil
	}
	alias.Visibility = "deleted"
	alias.UpdatedAt = time.Date(2026, 6, 28, 12, 2, 0, 0, time.UTC)
	m.aliases[slug] = alias
	return true, nil
}

func testConfig() Config {
	return Config{
		B2PublicBaseURL: "https://bucket.s3.us-west-004.backblazeb2.com",
		PublicBaseURL:   "https://share.doesthings.online",
		ObjectPrefix:    "s",
		MaxUploadBytes:  1024,
		PresignTTL:      15 * time.Minute,
		UploadTokenTTL:  time.Hour,
		UploadTokenKey:  []byte("01234567890123456789012345678901"),
		AliasHMACKey:    []byte("alias-key-012345678901234567890123"),
		SessionTTL:      12 * time.Hour,
		SessionAuthKey:  []byte("abcdefghijklmnopqrstuvwxyz012345"),
		Clock:           func() time.Time { return time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC) },
	}
}

func authenticatedFakeAuth(subject string) fakeAuth {
	return fakeAuth{authenticated: AuthenticatedRequest{
		Principal: Principal{Subject: subject},
		CSRFToken: "csrf-token",
	}}
}

func setCSRF(request *http.Request) {
	request.Header.Set(csrfHeaderName, "csrf-token")
}

func expectedSlug(t *testing.T, cfg Config, extension string) string {
	t.Helper()
	_, bytes, err := NormalizeSHA256(testSHA256)
	if err != nil {
		t.Fatal(err)
	}
	return GenerateAliasSlug(cfg.AliasHMACKey, bytes, extension)
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, newMemoryMetadata(), slog.Default())
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

func TestCreateUploadUsesContentAddressedObjectAndAlias(t *testing.T) {
	cfg := testConfig()
	store := &fakeStore{presign: PresignedUpload{
		URL:    "https://upload.example.test/presigned",
		Header: http.Header{"Content-Type": []string{"image/png"}, "Host": []string{"ignored"}},
	}}
	server := NewServer(cfg, authenticatedFakeAuth("user-1"), store, newMemoryMetadata(), slog.Default())
	body := `{"filename":"Screenshot 1.png","contentType":"image/png","size":42,"sha256":"` + testSHA256 + `"}`
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader(body))
	setCSRF(request)
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
	if response.ObjectKey != "s/"+testSHA256+".png" || store.presignedKey != response.ObjectKey {
		t.Fatalf("object key = %q, presigned = %q", response.ObjectKey, store.presignedKey)
	}
	wantPublic := "https://share.doesthings.online/s/" + expectedSlug(t, cfg, ".png")
	if response.PublicURL != wantPublic {
		t.Fatalf("public URL = %q, want %q", response.PublicURL, wantPublic)
	}
	if response.B2URL != "https://bucket.s3.us-west-004.backblazeb2.com/s/"+testSHA256+".png" {
		t.Fatalf("b2 URL = %q", response.B2URL)
	}
	if response.UploadToken == "" || response.AlreadyUploaded {
		t.Fatalf("response = %#v", response)
	}
}

func TestCreateUploadReturnsAliasOnlyWhenObjectExists(t *testing.T) {
	cfg := testConfig()
	metadata := newMemoryMetadata()
	metadata.objects[testSHA256] = StoredObject{
		SHA256:      testSHA256,
		ObjectKey:   "s/" + testSHA256 + ".png",
		Size:        42,
		ContentType: "image/png",
		Extension:   ".png",
		Uploader:    "user-1",
	}
	store := &fakeStore{}
	server := NewServer(cfg, authenticatedFakeAuth("user-1"), store, metadata, slog.Default())
	body := `{"filename":"again.png","contentType":"image/png","size":42,"sha256":"` + testSHA256 + `"}`
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader(body))
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response createUploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.AlreadyUploaded || response.UploadURL != "" || response.UploadToken != "" || store.presignedKey != "" {
		t.Fatalf("response = %#v, presigned = %q", response, store.presignedKey)
	}
	if _, ok := metadata.aliases[expectedSlug(t, cfg, ".png")]; !ok {
		t.Fatalf("expected alias to be recorded: %#v", metadata.aliases)
	}
}

func TestCreateUploadAllowsManualAliasRepoint(t *testing.T) {
	cfg := testConfig()
	metadata := newMemoryMetadata()
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".png", Size: 42, ContentType: "image/png", Extension: ".png"}
	metadata.aliases["latest.png"] = ShareAlias{Slug: "latest.png", ObjectSHA256: strings.Repeat("a", 64), Owner: "user-1"}
	server := NewServer(cfg, authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	body := `{"filename":"again.png","contentType":"image/png","size":42,"sha256":"` + testSHA256 + `","alias":"latest"}`
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader(body))
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if metadata.aliases["latest.png"].ObjectSHA256 != testSHA256 {
		t.Fatalf("alias was not repointed: %#v", metadata.aliases["latest.png"])
	}
	if len(metadata.history) != 1 {
		t.Fatalf("history = %#v", metadata.history)
	}
}

func TestCreateUploadRejectsUnauthenticated(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, newMemoryMetadata(), slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader(`{"filename":"a.txt","size":1}`))
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestCreateUploadRejectsMissingCSRFToken(t *testing.T) {
	server := NewServer(testConfig(), authenticatedFakeAuth("user-1"), &fakeStore{}, newMemoryMetadata(), slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader(`{"filename":"a.txt","size":1}`))
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestCreateUploadRejectsInvalidSHA256(t *testing.T) {
	server := NewServer(testConfig(), authenticatedFakeAuth("user-1"), &fakeStore{}, newMemoryMetadata(), slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader(`{"filename":"a.bin","size":1,"sha256":"nope"}`))
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestCreateUploadRejectsOversizedFile(t *testing.T) {
	server := NewServer(testConfig(), authenticatedFakeAuth("user-1"), &fakeStore{}, newMemoryMetadata(), slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader(`{"filename":"a.bin","size":2048,"sha256":"`+testSHA256+`"}`))
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestCompleteUploadReturnsVerifiedMetadata(t *testing.T) {
	cfg := testConfig()
	slug := expectedSlug(t, cfg, ".txt")
	token, err := SignUploadToken(cfg.UploadTokenKey, uploadTokenPayload{
		ObjectKey:       "s/" + testSHA256 + ".txt",
		SHA256:          testSHA256,
		AliasSlug:       slug,
		DisplayFilename: "a.txt",
		ContentType:     "text/plain",
		Extension:       ".txt",
		Size:            12,
		Subject:         "user-1",
		ExpiresAt:       cfg.Clock().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := newMemoryMetadata()
	server := NewServer(
		cfg,
		authenticatedFakeAuth("user-1"),
		&fakeStore{head: ObjectMetadata{ContentLength: 12, ContentType: "text/plain", ETag: "abc123"}},
		metadata,
		slog.Default(),
	)
	request := httptest.NewRequest(http.MethodPost, "/api/uploads/complete", strings.NewReader(`{"uploadToken":"`+token+`"}`))
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response completeUploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Verified || response.Size != 12 || response.ETag != "abc123" || response.SHA256 != testSHA256 {
		t.Fatalf("response = %#v", response)
	}
	if response.PublicURL != "https://share.doesthings.online/s/"+slug {
		t.Fatalf("public URL = %q", response.PublicURL)
	}
	if _, ok := metadata.objects[testSHA256]; !ok {
		t.Fatalf("object was not recorded")
	}
	if _, ok := metadata.aliases[slug]; !ok {
		t.Fatalf("alias was not recorded")
	}
}

func TestCompleteUploadRequiresHeadVerification(t *testing.T) {
	cfg := testConfig()
	token, err := SignUploadToken(cfg.UploadTokenKey, uploadTokenPayload{
		ObjectKey: "s/" + testSHA256 + ".txt",
		SHA256:    testSHA256,
		AliasSlug: expectedSlug(t, cfg, ".txt"),
		Size:      12,
		Subject:   "user-1",
		ExpiresAt: cfg.Clock().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(
		cfg,
		authenticatedFakeAuth("user-1"),
		&fakeStore{headErr: errors.New("transient")},
		newMemoryMetadata(),
		slog.Default(),
	)
	request := httptest.NewRequest(http.MethodPost, "/api/uploads/complete", strings.NewReader(`{"uploadToken":"`+token+`"}`))
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPublicShareRedirectsToNativeB2URL(t *testing.T) {
	metadata := newMemoryMetadata()
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Size: 12, ContentType: "text/plain"}
	metadata.aliases["public.txt"] = ShareAlias{Slug: "public.txt", ObjectSHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Visibility: "public"}
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/s/public.txt", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	want := "https://bucket.s3.us-west-004.backblazeb2.com/s/" + testSHA256 + ".txt"
	if got := recorder.Header().Get("Location"); got != want {
		t.Fatalf("location = %q, want %q", got, want)
	}
	if metadata.aliases["public.txt"].RedirectCount != 1 {
		t.Fatalf("redirect count = %d", metadata.aliases["public.txt"].RedirectCount)
	}
}

func TestPublicShareHeadRedirectsToNativeB2URL(t *testing.T) {
	metadata := newMemoryMetadata()
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Size: 12, ContentType: "text/plain"}
	metadata.aliases["public.txt"] = ShareAlias{Slug: "public.txt", ObjectSHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Visibility: "public"}
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodHead, "/s/public.txt", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPublicShareRejectsUnknownOrNestedAliases(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, newMemoryMetadata(), slog.Default())
	for _, path := range []string{"/s/missing.txt", "/s/nested/path.txt"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want %d", path, recorder.Code, http.StatusNotFound)
		}
	}
}

func TestPublicShareRejectsDeletedAlias(t *testing.T) {
	metadata := newMemoryMetadata()
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Size: 12, ContentType: "text/plain"}
	metadata.aliases["deleted.txt"] = ShareAlias{Slug: "deleted.txt", ObjectSHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Visibility: "deleted"}
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/s/deleted.txt", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestListSharesReturnsAuthenticatedUserHistory(t *testing.T) {
	metadata := newMemoryMetadata()
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Size: 12, ContentType: "text/plain"}
	metadata.aliases["mine.txt"] = ShareAlias{Slug: "mine.txt", ObjectSHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Owner: "user-1", DisplayFilename: "mine.txt", Visibility: "public"}
	metadata.aliases["other.txt"] = ShareAlias{Slug: "other.txt", ObjectSHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Owner: "user-2", Visibility: "public"}
	metadata.aliases["deleted.txt"] = ShareAlias{Slug: "deleted.txt", ObjectSHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Owner: "user-1", Visibility: "deleted"}
	server := NewServer(testConfig(), authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/api/shares", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response listSharesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Shares) != 1 || response.Shares[0].PublicURL != "https://share.doesthings.online/s/mine.txt" {
		t.Fatalf("response = %#v", response)
	}
}

func TestDeleteShareSoftDeletesOwnedAlias(t *testing.T) {
	metadata := newMemoryMetadata()
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Size: 12, ContentType: "text/plain"}
	metadata.aliases["mine.txt"] = ShareAlias{Slug: "mine.txt", ObjectSHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Owner: "user-1", Visibility: "public"}
	server := NewServer(testConfig(), authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodDelete, "/api/shares/mine.txt", nil)
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if metadata.aliases["mine.txt"].Visibility != "deleted" {
		t.Fatalf("alias visibility = %q", metadata.aliases["mine.txt"].Visibility)
	}
}

func TestDeleteShareRequiresOwner(t *testing.T) {
	metadata := newMemoryMetadata()
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Size: 12, ContentType: "text/plain"}
	metadata.aliases["other.txt"] = ShareAlias{Slug: "other.txt", ObjectSHA256: testSHA256, ObjectKey: "s/" + testSHA256 + ".txt", Owner: "user-2", Visibility: "public"}
	server := NewServer(testConfig(), authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodDelete, "/api/shares/other.txt", nil)
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if metadata.aliases["other.txt"].Visibility != "public" {
		t.Fatalf("alias visibility = %q", metadata.aliases["other.txt"].Visibility)
	}
}

func TestDeleteShareRejectsMissingCSRFToken(t *testing.T) {
	metadata := newMemoryMetadata()
	server := NewServer(testConfig(), authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodDelete, "/api/shares/mine.txt", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestSessionEndpointReturnsAuthenticatedUserAndCSRF(t *testing.T) {
	server := NewServer(testConfig(), authenticatedFakeAuth("user-1"), &fakeStore{}, newMemoryMetadata(), slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response sessionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Authenticated || response.User.Subject != "user-1" || response.CSRFToken != "csrf-token" {
		t.Fatalf("response = %#v", response)
	}
}

func TestSessionEndpointReturnsAnonymousSessionState(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, newMemoryMetadata(), slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response sessionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Authenticated {
		t.Fatalf("response = %#v", response)
	}
}

func TestWebRoutesServeAppAndManifest(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, newMemoryMetadata(), slog.Default())

	for _, path := range []string{"/", "/share"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", path, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), `<link rel="manifest" href="/manifest.webmanifest">`) {
			t.Fatalf("%s did not serve the app shell", path)
		}
		if !strings.Contains(recorder.Body.String(), `<body class="auth-pending">`) {
			t.Fatalf("%s app shell should hide upload UI until auth check", path)
		}
		if strings.Contains(recorder.Body.String(), `>Sign in<`) {
			t.Fatalf("%s app shell should not render a manual sign-in button", path)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"share_target"`) || !strings.Contains(recorder.Body.String(), `"/share-target"`) {
		t.Fatalf("manifest missing share target: %s", recorder.Body.String())
	}
}

func TestShareTargetPostIsNotServerSideUploadFallback(t *testing.T) {
	server := NewServer(testConfig(), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, newMemoryMetadata(), slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/share-target", strings.NewReader("not uploaded here"))
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}
