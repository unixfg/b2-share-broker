package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
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
	headErr  error
	objects  map[string][]byte
	putKey   string
	putType  string
	deleted  []string
	headKeys []string
}

func (s *fakeStore) HeadObject(_ context.Context, key string) (ObjectMetadata, error) {
	s.headKeys = append(s.headKeys, key)
	if s.headErr != nil {
		return ObjectMetadata{}, s.headErr
	}
	if body, ok := s.objects[key]; ok {
		return ObjectMetadata{ContentLength: int64(len(body))}, nil
	}
	return ObjectMetadata{}, nil
}

func (s *fakeStore) DownloadObject(_ context.Context, key string, writer io.Writer) error {
	if s.objects == nil {
		return errors.New("object store is empty")
	}
	body, ok := s.objects[key]
	if !ok {
		return errors.New("object not found")
	}
	_, err := writer.Write(body)
	return err
}

func (s *fakeStore) PutObject(_ context.Context, key, contentType string, size int64, reader io.Reader) (ObjectMetadata, error) {
	if s.objects == nil {
		s.objects = map[string][]byte{}
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return ObjectMetadata{}, err
	}
	s.objects[key] = body
	s.putKey = key
	s.putType = contentType
	return ObjectMetadata{ContentLength: size, ContentType: contentType, ETag: "put-etag"}, nil
}

func (s *fakeStore) DeleteObject(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	if s.objects != nil {
		delete(s.objects, key)
	}
	return nil
}

type memoryMetadata struct {
	objects     map[string]StoredObject
	aliases     map[string]ShareAlias
	history     []ShareAlias
	jobs        map[string]ProcessingJob
	derivatives map[string]ObjectDerivative
	unavailable []string
}

func newMemoryMetadata() *memoryMetadata {
	return &memoryMetadata{
		objects:     map[string]StoredObject{},
		aliases:     map[string]ShareAlias{},
		jobs:        map[string]ProcessingJob{},
		derivatives: map[string]ObjectDerivative{},
	}
}

func (m *memoryMetadata) GetObject(_ context.Context, sha256 string) (StoredObject, bool, error) {
	object, ok := m.objects[sha256]
	return object, ok, nil
}

func (m *memoryMetadata) GetDerivedObject(_ context.Context, sourceSHA256, profile string) (StoredObject, bool, error) {
	derivative, ok := m.derivatives[sourceSHA256+"|"+profile]
	if !ok {
		return StoredObject{}, false, nil
	}
	object, ok := m.objects[derivative.TargetSHA256]
	if !ok || object.Status != "ready" {
		return StoredObject{}, false, nil
	}
	return object, true, nil
}

func (m *memoryMetadata) MarkObjectUnavailable(_ context.Context, sha256, status string) error {
	object, ok := m.objects[sha256]
	if !ok {
		return nil
	}
	if status == "" {
		status = "missing"
	}
	object.Status = status
	object.DeletedAt = time.Date(2026, 6, 28, 12, 9, 0, 0, time.UTC)
	m.objects[sha256] = object
	m.unavailable = append(m.unavailable, sha256)
	return nil
}

func (m *memoryMetadata) UpsertAlias(_ context.Context, alias ShareAlias) error {
	return m.upsertAlias(alias)
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

func (m *memoryMetadata) ListAliases(_ context.Context, owner, query string, limit int) ([]ShareAlias, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var aliases []ShareAlias
	for _, alias := range m.aliases {
		if alias.Owner != owner || alias.Visibility == "deleted" {
			continue
		}
		if query != "" && !aliasMatchesQuery(alias, query) {
			continue
		}
		aliases = append(aliases, alias)
		if len(aliases) >= limit {
			break
		}
	}
	return aliases, nil
}

func aliasMatchesQuery(alias ShareAlias, query string) bool {
	return strings.Contains(strings.ToLower(alias.Slug), query) ||
		strings.Contains(strings.ToLower(alias.DisplayFilename), query) ||
		strings.Contains(strings.ToLower(alias.Status), query) ||
		strings.Contains(strings.ToLower(alias.ContentType), query)
}

func (m *memoryMetadata) DeleteAlias(_ context.Context, slug, owner string) (DeletedShare, bool, error) {
	alias, ok := m.aliases[slug]
	if !ok || alias.Owner != owner || alias.Visibility == "deleted" {
		return DeletedShare{}, false, nil
	}
	alias.Visibility = "deleted"
	alias.UpdatedAt = time.Date(2026, 6, 28, 12, 2, 0, 0, time.UTC)
	m.aliases[slug] = alias

	deleted := DeletedShare{Alias: alias}
	for id, job := range m.jobs {
		if job.AliasSlug != slug || job.Owner != owner {
			continue
		}
		if job.Status == ProcessingStatusQueued || job.Status == ProcessingStatusRunning {
			job.Status = ProcessingStatusCanceled
			job.Error = "share deleted"
			m.jobs[id] = job
			if job.StagingPath != "" {
				deleted.StagingPaths = append(deleted.StagingPaths, job.StagingPath)
			}
		}
	}
	if alias.ObjectSHA256 != "" && alias.ObjectKey != "" {
		references := 0
		for _, other := range m.aliases {
			if other.ObjectSHA256 == alias.ObjectSHA256 && other.Visibility != "deleted" {
				references++
			}
		}
		if references == 0 {
			object := m.objects[alias.ObjectSHA256]
			object.Status = "deleted"
			object.DeletedAt = time.Date(2026, 6, 28, 12, 2, 0, 0, time.UTC)
			m.objects[alias.ObjectSHA256] = object
			deleted.ObjectKey = alias.ObjectKey
			deleted.ThumbnailKey = alias.ThumbnailKey
		}
	}
	return deleted, true, nil
}

func (m *memoryMetadata) CreateIngestJob(_ context.Context, job ProcessingJob, alias ShareAlias) (ProcessingJob, error) {
	alias.Status = AliasStatusPending
	alias.Visibility = "public"
	if err := m.upsertAlias(alias); err != nil {
		return ProcessingJob{}, err
	}
	now := time.Date(2026, 6, 28, 12, 3, 0, 0, time.UTC)
	job.Status = ProcessingStatusQueued
	job.CreatedAt = now
	job.UpdatedAt = now
	m.jobs[job.ID] = job
	return job, nil
}

func (m *memoryMetadata) GetProcessingJob(_ context.Context, id, owner string) (ProcessingJob, bool, error) {
	job, ok := m.jobs[id]
	if !ok || (owner != "" && job.Owner != owner) {
		return ProcessingJob{}, false, nil
	}
	return job, true, nil
}

func (m *memoryMetadata) ClaimNextProcessingJob(_ context.Context, worker string) (ProcessingJob, bool, error) {
	for id, job := range m.jobs {
		if job.Status != ProcessingStatusQueued {
			continue
		}
		job.Status = ProcessingStatusRunning
		job.UpdatedAt = time.Date(2026, 6, 28, 12, 4, 0, 0, time.UTC)
		job.StartedAt = job.UpdatedAt
		m.jobs[id] = job
		return job, true, nil
	}
	return ProcessingJob{}, false, nil
}

func (m *memoryMetadata) CompleteProcessingJob(_ context.Context, id string, object StoredObject, alias ShareAlias) error {
	job, ok := m.jobs[id]
	if !ok || job.Status != ProcessingStatusRunning {
		return errors.New("running job not found")
	}
	if object.Status == "" {
		object.Status = "ready"
	}
	m.objects[object.SHA256] = object
	if err := m.upsertAlias(alias); err != nil {
		return err
	}
	job.Status = ProcessingStatusCompleted
	job.TargetSHA256 = object.SHA256
	job.TargetObjectKey = object.ObjectKey
	job.CompletedAt = time.Date(2026, 6, 28, 12, 5, 0, 0, time.UTC)
	job.UpdatedAt = job.CompletedAt
	m.jobs[id] = job
	if job.SourceSHA256 != "" {
		m.derivatives[job.SourceSHA256+"|"+job.Profile] = ObjectDerivative{
			SourceSHA256: job.SourceSHA256,
			TargetSHA256: object.SHA256,
			Profile:      job.Profile,
			JobID:        id,
			CreatedAt:    job.CompletedAt,
		}
	}
	return nil
}

func (m *memoryMetadata) FailProcessingJob(_ context.Context, id, message string) error {
	job, ok := m.jobs[id]
	if !ok {
		return nil
	}
	job.Status = ProcessingStatusFailed
	job.Error = message
	job.CompletedAt = time.Date(2026, 6, 28, 12, 5, 0, 0, time.UTC)
	job.UpdatedAt = job.CompletedAt
	m.jobs[id] = job
	if alias, ok := m.aliases[job.AliasSlug]; ok && alias.Visibility != "deleted" {
		alias.Status = AliasStatusFailed
		alias.Error = message
		m.aliases[job.AliasSlug] = alias
	}
	return nil
}

func (m *memoryMetadata) upsertAlias(alias ShareAlias) error {
	if previous, ok := m.aliases[alias.Slug]; ok {
		if previous.Owner != alias.Owner {
			return ErrAliasConflict
		}
		if previous.ObjectSHA256 != "" && alias.ObjectSHA256 != "" && previous.ObjectSHA256 != alias.ObjectSHA256 {
			m.history = append(m.history, previous)
		}
		alias.CreatedAt = previous.CreatedAt
		alias.RedirectCount = previous.RedirectCount
		alias.LastRedirectedAt = previous.LastRedirectedAt
	}
	if alias.Visibility == "" {
		alias.Visibility = "public"
	}
	if alias.Status == "" {
		if alias.ObjectSHA256 == "" {
			alias.Status = AliasStatusPending
		} else {
			alias.Status = AliasStatusReady
		}
	}
	if object, ok := m.objects[alias.ObjectSHA256]; ok {
		alias.ObjectKey = object.ObjectKey
		alias.Size = object.Size
		alias.ContentType = object.ContentType
		alias.Width = object.Width
		alias.Height = object.Height
		alias.ThumbnailKey = object.ThumbnailKey
	}
	if alias.CreatedAt.IsZero() {
		alias.CreatedAt = time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	}
	alias.UpdatedAt = time.Date(2026, 6, 28, 12, 6, 0, 0, time.UTC)
	m.aliases[alias.Slug] = alias
	return nil
}

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		B2PublicBaseURL: "https://bucket.s3.us-west-004.backblazeb2.com",
		PublicBaseURL:   "https://share.doesthings.online",
		MaxUploadBytes:  1024,
		SessionTTL:      12 * time.Hour,
		SessionAuthKey:  []byte("abcdefghijklmnopqrstuvwxyz012345"),
		StagingDir:      t.TempDir(),
		Clock:           func() time.Time { return time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC) },
	}
}

func authenticatedFakeAuth(subject string) fakeAuth {
	return fakeAuth{authenticated: AuthenticatedRequest{
		Principal:    Principal{Subject: subject},
		CSRFToken:    "csrf-token",
		RequiresCSRF: true,
	}}
}

func bearerFakeAuth(subject string) fakeAuth {
	return fakeAuth{authenticated: AuthenticatedRequest{
		Principal: Principal{Subject: subject},
	}}
}

func setCSRF(request *http.Request) {
	request.Header.Set(csrfHeaderName, "csrf-token")
}

func multipartUpload(t *testing.T, fieldName, filename, contentType string, body []byte, alias string) (*bytes.Buffer, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	part, err := writer.CreatePart(textprotoMIMEHeader(map[string]string{
		"Content-Disposition": `form-data; name="` + fieldName + `"; filename="` + filename + `"`,
		"Content-Type":        contentType,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if alias != "" {
		if err := writer.WriteField("alias", alias); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &buffer, writer.FormDataContentType()
}

func textprotoMIMEHeader(values map[string]string) textproto.MIMEHeader {
	header := textproto.MIMEHeader{}
	for key, value := range values {
		header.Set(key, value)
	}
	return header
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	server := NewServer(testConfig(t), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, newMemoryMetadata(), slog.Default())
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

func TestCreateUploadStagesMultipartAndQueuesShare(t *testing.T) {
	cfg := testConfig(t)
	metadata := newMemoryMetadata()
	server := NewServer(cfg, authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	body, contentType := multipartUpload(t, "file", "Screenshot 1.png", "image/png", []byte("png data"), "")
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", body)
	request.Header.Set("Content-Type", contentType)
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response createUploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ShareURL != "https://share.doesthings.online/s/"+response.Slug ||
		!strings.HasPrefix(response.Slug, "screenshot_1-") ||
		!strings.HasSuffix(response.Slug, ".png") {
		t.Fatalf("response = %#v", response)
	}
	job := metadata.jobs[response.JobID]
	if job.Status != ProcessingStatusQueued || job.Profile != ProcessingProfileUploadFinalize || job.Owner != "user-1" {
		t.Fatalf("job = %#v", job)
	}
	staged, err := os.ReadFile(job.StagingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != "png data" {
		t.Fatalf("staged = %q", staged)
	}
	alias := metadata.aliases[response.Slug]
	if alias.Status != AliasStatusPending || alias.ObjectSHA256 != "" {
		t.Fatalf("alias = %#v", alias)
	}
}

func TestCreateUploadVideoQueuesMP4Normalization(t *testing.T) {
	cfg := testConfig(t)
	metadata := newMemoryMetadata()
	server := NewServer(cfg, authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	body, contentType := multipartUpload(t, "file", "Clip.mov", "video/quicktime", []byte("mov data"), "")
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", body)
	request.Header.Set("Content-Type", contentType)
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response createUploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(response.Slug, "clip-") || !strings.HasSuffix(response.Slug, ".mp4") {
		t.Fatalf("slug = %q", response.Slug)
	}
	job := metadata.jobs[response.JobID]
	if job.Profile != ProcessingProfileMP4Web || job.SourceType != "video/quicktime" {
		t.Fatalf("job = %#v", job)
	}
	if job.SourceSHA256 != sha256Hex([]byte("mov data")) {
		t.Fatalf("job source sha256 = %q, want %q", job.SourceSHA256, sha256Hex([]byte("mov data")))
	}
}

func TestCreateUploadStreamsLargeFileWithoutMultipartTemp(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxUploadBytes = 32 << 20
	t.Setenv("TMPDIR", t.TempDir()+"/missing")
	metadata := newMemoryMetadata()
	server := NewServer(cfg, authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	payload := bytes.Repeat([]byte("x"), 17<<20)
	body, contentType := multipartUpload(t, "file", "large.bin", "application/octet-stream", payload, "")
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", body)
	request.Header.Set("Content-Type", contentType)
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response createUploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	job := metadata.jobs[response.JobID]
	if job.SourceSize != int64(len(payload)) {
		t.Fatalf("job size = %d, want %d", job.SourceSize, len(payload))
	}
	staged, err := os.Stat(job.StagingPath)
	if err != nil {
		t.Fatal(err)
	}
	if staged.Size() != int64(len(payload)) {
		t.Fatalf("staged size = %d, want %d", staged.Size(), len(payload))
	}
}

func TestCreateUploadRejectsNonMultipartBody(t *testing.T) {
	cfg := testConfig(t)
	metadata := newMemoryMetadata()
	server := NewServer(cfg, authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader(`{"file":"nope"}`))
	request.Header.Set("Content-Type", "application/json")
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "multipart/form-data file upload is required") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if len(metadata.jobs) != 0 {
		t.Fatalf("jobs = %#v", metadata.jobs)
	}
}

func TestCreateUploadReportsMultipartBodyTooLarge(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxUploadBytes = 1
	metadata := newMemoryMetadata()
	server := NewServer(cfg, authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	body, contentType := multipartUpload(t, "file", "large.bin", "application/octet-stream", bytes.Repeat([]byte("x"), 17<<20), "")
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", body)
	request.Header.Set("Content-Type", contentType)
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "file is larger than the configured maximum") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if len(metadata.jobs) != 0 {
		t.Fatalf("jobs = %#v", metadata.jobs)
	}
}

func TestCreateUploadRejectsCustomAliasField(t *testing.T) {
	cfg := testConfig(t)
	metadata := newMemoryMetadata()
	server := NewServer(cfg, authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	body, contentType := multipartUpload(t, "file", "demo.txt", "text/plain", []byte("replacement"), "demo")
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", body)
	request.Header.Set("Content-Type", contentType)
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(metadata.jobs) != 0 {
		t.Fatalf("jobs = %#v", metadata.jobs)
	}
	entries, err := os.ReadDir(cfg.StagingDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging entries = %d, want 0", len(entries))
	}
}

func TestCreateUploadRequiresCSRFForSessionButNotBearer(t *testing.T) {
	cfg := testConfig(t)
	body, contentType := multipartUpload(t, "file", "a.txt", "text/plain", []byte("hello"), "")
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", body)
	request.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	NewServer(cfg, authenticatedFakeAuth("user-1"), &fakeStore{}, newMemoryMetadata(), slog.Default()).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("session status = %d, want %d", recorder.Code, http.StatusForbidden)
	}

	body, contentType = multipartUpload(t, "file", "a.txt", "text/plain", []byte("hello"), "")
	request = httptest.NewRequest(http.MethodPost, "/api/uploads", body)
	request.Header.Set("Content-Type", contentType)
	recorder = httptest.NewRecorder()
	NewServer(cfg, bearerFakeAuth("user-1"), &fakeStore{}, newMemoryMetadata(), slog.Default()).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("bearer status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestGetUploadStatusRequiresOwner(t *testing.T) {
	metadata := newMemoryMetadata()
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-2", AliasSlug: "share.txt", Profile: ProcessingProfileUploadFinalize, Status: ProcessingStatusQueued}
	server := NewServer(testConfig(t), authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/api/uploads/job-1", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestPublicShareStatesAndRedirect(t *testing.T) {
	cfg := testConfig(t)
	metadata := newMemoryMetadata()
	key := "01/" + testSHA256 + ".txt"
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: key, Size: 12, ContentType: "text/plain", Status: "ready"}
	metadata.aliases["pending.txt"] = ShareAlias{Slug: "pending.txt", Owner: "user-1", Visibility: "public", Status: AliasStatusPending}
	metadata.aliases["failed.txt"] = ShareAlias{Slug: "failed.txt", Owner: "user-1", Visibility: "public", Status: AliasStatusFailed}
	metadata.aliases["ready.txt"] = ShareAlias{Slug: "ready.txt", ObjectSHA256: testSHA256, ObjectKey: key, Owner: "user-1", Visibility: "public", Status: AliasStatusReady, ContentType: "text/plain"}
	server := NewServer(cfg, fakeAuth{err: ErrUnauthorized}, &fakeStore{}, metadata, slog.Default())

	for _, item := range []struct {
		path string
		want int
	}{
		{"/s/pending.txt", http.StatusAccepted},
		{"/s/failed.txt", http.StatusServiceUnavailable},
		{"/s/ready.txt", http.StatusFound},
	} {
		request := httptest.NewRequest(http.MethodGet, item.path, nil)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != item.want {
			t.Fatalf("%s status = %d, want %d", item.path, recorder.Code, item.want)
		}
		if item.want == http.StatusFound && recorder.Header().Get("Location") != "https://bucket.s3.us-west-004.backblazeb2.com/"+key {
			t.Fatalf("location = %q", recorder.Header().Get("Location"))
		}
		if item.want == http.StatusFound {
			if recorder.Header().Get("Content-Type") != "text/plain" {
				t.Fatalf("content-type = %q", recorder.Header().Get("Content-Type"))
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("redirect body = %q", recorder.Body.String())
			}
		}
	}
	if metadata.aliases["ready.txt"].RedirectCount != 1 {
		t.Fatalf("redirect count = %d", metadata.aliases["ready.txt"].RedirectCount)
	}
}

func TestPublicShareServesUnfurlPageToCrawlers(t *testing.T) {
	cfg := testConfig(t)
	metadata := newMemoryMetadata()
	key := "01/" + testSHA256 + ".mp4"
	thumbnailKey := "01/" + testSHA256 + ".jpg"
	b2MediaURL := "https://bucket.s3.us-west-004.backblazeb2.com/" + key
	ogMediaURL := "https://share.doesthings.online/s/ready.mp4/media"
	ogThumbnailURL := "https://share.doesthings.online/s/ready.mp4/thumbnail"
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: key, Size: 12582912, ContentType: "video/mp4", Status: "ready", Width: 1920, Height: 1080, ThumbnailKey: thumbnailKey}
	metadata.aliases["ready.mp4"] = ShareAlias{Slug: "ready.mp4", ObjectSHA256: testSHA256, ObjectKey: key, Owner: "user-1", DisplayFilename: "Launch Clip.mp4", Visibility: "public", Status: AliasStatusReady, ContentType: "video/mp4", Size: 12582912, Width: 1920, Height: 1080, ThumbnailKey: thumbnailKey}
	server := NewServer(cfg, fakeAuth{err: ErrUnauthorized}, &fakeStore{}, metadata, slog.Default())

	request := httptest.NewRequest(http.MethodGet, "/s/ready.mp4", nil)
	request.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); location != "" {
		t.Fatalf("unexpected redirect to %q", location)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`property="og:title" content="Launch Clip.mp4"`,
		`property="og:type" content="video.other"`,
		`property="og:url" content="https://share.doesthings.online/s/ready.mp4"`,
		`property="og:site_name" content="share.doesthings.online"`,
		`property="og:video" content="` + ogMediaURL + `"`,
		`property="og:video:secure_url" content="` + ogMediaURL + `"`,
		`property="og:video:type" content="video/mp4"`,
		`property="og:video:width" content="1920"`,
		`property="og:video:height" content="1080"`,
		`property="og:image" content="` + ogThumbnailURL + `"`,
		`name="theme-color"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if metadata.aliases["ready.mp4"].RedirectCount != 0 {
		t.Fatalf("crawler fetch counted as redirect: %d", metadata.aliases["ready.mp4"].RedirectCount)
	}

	request = httptest.NewRequest(http.MethodGet, "/s/ready.mp4", nil)
	request.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != b2MediaURL {
		t.Fatalf("browser status = %d, location = %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestPublicShareMediaVariantRedirectsAndCounts(t *testing.T) {
	cfg := testConfig(t)
	metadata := newMemoryMetadata()
	key := "01/" + testSHA256 + ".mp4"
	thumbnailKey := "01/" + testSHA256 + ".jpg"
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: key, Size: 12582912, ContentType: "video/mp4", Status: "ready", Width: 1920, Height: 1080, ThumbnailKey: thumbnailKey}
	metadata.aliases["ready.mp4"] = ShareAlias{Slug: "ready.mp4", ObjectSHA256: testSHA256, ObjectKey: key, Owner: "user-1", DisplayFilename: "Launch Clip.mp4", Visibility: "public", Status: AliasStatusReady, ContentType: "video/mp4", Size: 12582912, Width: 1920, Height: 1080, ThumbnailKey: thumbnailKey}
	metadata.aliases["pending.mp4"] = ShareAlias{Slug: "pending.mp4", Owner: "user-1", DisplayFilename: "pending.mp4", Visibility: "public", Status: AliasStatusPending}
	metadata.aliases["plain.txt"] = ShareAlias{Slug: "plain.txt", ObjectSHA256: testSHA256, ObjectKey: key, Owner: "user-1", Visibility: "public", Status: AliasStatusReady, ContentType: "text/plain"}
	server := NewServer(cfg, fakeAuth{err: ErrUnauthorized}, &fakeStore{}, metadata, slog.Default())

	request := httptest.NewRequest(http.MethodGet, "/s/ready.mp4/media", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "https://bucket.s3.us-west-004.backblazeb2.com/"+key {
		t.Fatalf("media status = %d, location = %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if metadata.aliases["ready.mp4"].RedirectCount != 1 {
		t.Fatalf("redirect count = %d, want 1", metadata.aliases["ready.mp4"].RedirectCount)
	}

	request = httptest.NewRequest(http.MethodGet, "/s/ready.mp4/thumbnail", nil)
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "https://bucket.s3.us-west-004.backblazeb2.com/"+thumbnailKey {
		t.Fatalf("thumbnail status = %d, location = %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if metadata.aliases["ready.mp4"].RedirectCount != 1 {
		t.Fatalf("thumbnail fetch counted: redirect count = %d, want 1", metadata.aliases["ready.mp4"].RedirectCount)
	}

	for _, path := range []string{"/s/pending.mp4/media", "/s/plain.txt/thumbnail", "/s/missing.mp4/media"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want %d", path, recorder.Code, http.StatusNotFound)
		}
	}
}

func TestPublicShareUnfurlPageOmitsOptionalTags(t *testing.T) {
	cfg := testConfig(t)
	metadata := newMemoryMetadata()
	key := "01/" + testSHA256 + ".txt"
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: key, Size: 12, ContentType: "text/plain", Status: "ready"}
	metadata.aliases["ready.txt"] = ShareAlias{Slug: "ready.txt", ObjectSHA256: testSHA256, ObjectKey: key, Owner: "user-1", DisplayFilename: "notes.txt", Visibility: "public", Status: AliasStatusReady, ContentType: "text/plain", Size: 12}
	server := NewServer(cfg, fakeAuth{err: ErrUnauthorized}, &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/s/ready.txt", nil)
	request.Header.Set("User-Agent", "Slackbot-LinkExpanding 1.0")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, unwanted := range []string{"og:video", "og:image", "og:video:width"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("body unexpectedly contains %q:\n%s", unwanted, body)
		}
	}
	for _, want := range []string{`property="og:title" content="notes.txt"`, `property="og:type" content="website"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestPublicShareCORSAllowsConfiguredOrigin(t *testing.T) {
	cfg := testConfig(t)
	cfg.PublicShareCORSAllowedOrigins = []string{"https://discord.com"}
	metadata := newMemoryMetadata()
	key := "01/" + testSHA256 + ".mp4"
	metadata.aliases["ready.mp4"] = ShareAlias{Slug: "ready.mp4", ObjectSHA256: testSHA256, ObjectKey: key, Owner: "user-1", Visibility: "public", Status: AliasStatusReady, ContentType: "video/mp4"}
	server := NewServer(cfg, fakeAuth{err: ErrUnauthorized}, &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/s/ready.mp4", nil)
	request.Header.Set("Origin", "https://discord.com")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d", recorder.Code)
	}
	if recorder.Header().Get("Access-Control-Allow-Origin") != "https://discord.com" {
		t.Fatalf("allow origin = %q", recorder.Header().Get("Access-Control-Allow-Origin"))
	}
	if recorder.Header().Get("Access-Control-Expose-Headers") == "" {
		t.Fatal("expected exposed headers")
	}
	if recorder.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("content-type = %q", recorder.Header().Get("Content-Type"))
	}
}

func TestPublicShareCORSPreflightAndDisallowedOrigin(t *testing.T) {
	cfg := testConfig(t)
	cfg.PublicShareCORSAllowedOrigins = []string{"https://discord.com"}
	server := NewServer(cfg, fakeAuth{err: ErrUnauthorized}, &fakeStore{}, newMemoryMetadata(), slog.Default())
	request := httptest.NewRequest(http.MethodOptions, "/s/ready.mp4", nil)
	request.Header.Set("Origin", "https://discord.com")
	request.Header.Set("Access-Control-Request-Method", "GET")
	request.Header.Set("Access-Control-Request-Headers", "range")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	if recorder.Header().Get("Access-Control-Allow-Origin") != "https://discord.com" {
		t.Fatalf("allow origin = %q", recorder.Header().Get("Access-Control-Allow-Origin"))
	}
	if recorder.Header().Get("Access-Control-Allow-Headers") != "Range" {
		t.Fatalf("allow headers = %q", recorder.Header().Get("Access-Control-Allow-Headers"))
	}

	request = httptest.NewRequest(http.MethodOptions, "/s/ready.mp4", nil)
	request.Header.Set("Origin", "https://example.com")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("disallowed origin was allowed: %q", recorder.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestListSharesReturnsPendingAndReadyHistory(t *testing.T) {
	metadata := newMemoryMetadata()
	key := "01/" + testSHA256 + ".txt"
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: key, Size: 12, ContentType: "text/plain", Status: "ready"}
	metadata.aliases["pending.txt"] = ShareAlias{Slug: "pending.txt", Owner: "user-1", DisplayFilename: "pending.txt", Visibility: "public", Status: AliasStatusPending}
	metadata.aliases["ready.txt"] = ShareAlias{Slug: "ready.txt", ObjectSHA256: testSHA256, ObjectKey: key, Owner: "user-1", DisplayFilename: "ready.txt", Visibility: "public", Status: AliasStatusReady}
	metadata.aliases["deleted.txt"] = ShareAlias{Slug: "deleted.txt", Owner: "user-1", Visibility: "deleted", Status: AliasStatusReady}
	server := NewServer(testConfig(t), authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
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
	if len(response.Shares) != 2 {
		t.Fatalf("response = %#v", response)
	}
	for _, share := range response.Shares {
		if share.PublicURL != "https://share.doesthings.online/s/"+share.Slug {
			t.Fatalf("share = %#v", share)
		}
		if share.Slug == "ready.txt" && share.B2URL != "https://bucket.s3.us-west-004.backblazeb2.com/"+key {
			t.Fatalf("share = %#v", share)
		}
	}
}

func TestListSharesSearchesOwnedVisibleHistory(t *testing.T) {
	metadata := newMemoryMetadata()
	metadata.aliases["video-1234.mp4"] = ShareAlias{Slug: "video-1234.mp4", Owner: "user-1", DisplayFilename: "Launch Clip.mp4", Visibility: "public", Status: AliasStatusReady, ContentType: "video/mp4"}
	metadata.aliases["notes-1234.txt"] = ShareAlias{Slug: "notes-1234.txt", Owner: "user-1", DisplayFilename: "notes.txt", Visibility: "public", Status: AliasStatusReady, ContentType: "text/plain"}
	metadata.aliases["deleted-video.mp4"] = ShareAlias{Slug: "deleted-video.mp4", Owner: "user-1", DisplayFilename: "Launch Clip.mp4", Visibility: "deleted", Status: AliasStatusReady, ContentType: "video/mp4"}
	metadata.aliases["other-video.mp4"] = ShareAlias{Slug: "other-video.mp4", Owner: "user-2", DisplayFilename: "Launch Clip.mp4", Visibility: "public", Status: AliasStatusReady, ContentType: "video/mp4"}
	server := NewServer(testConfig(t), authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/api/shares?q=video&limit=100", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response listSharesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Shares) != 1 || response.Shares[0].Slug != "video-1234.mp4" {
		t.Fatalf("response = %#v", response)
	}
}

func TestListSharesSearchMatchesStatusAndHonorsLimit(t *testing.T) {
	metadata := newMemoryMetadata()
	metadata.aliases["failed.txt"] = ShareAlias{Slug: "failed.txt", Owner: "user-1", DisplayFilename: "failed.txt", Visibility: "public", Status: AliasStatusFailed}
	metadata.aliases["failed-again.txt"] = ShareAlias{Slug: "failed-again.txt", Owner: "user-1", DisplayFilename: "failed-again.txt", Visibility: "public", Status: AliasStatusFailed}
	server := NewServer(testConfig(t), authenticatedFakeAuth("user-1"), &fakeStore{}, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/api/shares?q=failed&limit=1", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response listSharesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Shares) != 1 {
		t.Fatalf("response = %#v", response)
	}
}

func TestParseShareLimitDefaultsAndCaps(t *testing.T) {
	tests := map[string]int{
		"":     50,
		"nope": 50,
		"0":    50,
		"-1":   50,
		"25":   25,
		"101":  100,
	}
	for value, want := range tests {
		if got := parseShareLimit(value); got != want {
			t.Fatalf("parseShareLimit(%q) = %d, want %d", value, got, want)
		}
	}
}

func TestWebRoutesServeLandingDocsAndShareApp(t *testing.T) {
	server := NewServer(testConfig(t), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, newMemoryMetadata(), slog.Default())
	tests := []struct {
		path string
		want string
	}{
		{"/", "B2 Share"},
		{"/docs", "POST"},
		{"/docs/", "DELETE"},
		{"/share", "fileInput"},
		{"/manifest.webmanifest", "share_target"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", test.path, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), test.want) {
			t.Fatalf("%s body did not contain %q", test.path, test.want)
		}
	}
}

func TestDeleteShareRemovesUnreferencedB2ObjectAndPreservesStats(t *testing.T) {
	metadata := newMemoryMetadata()
	key := "01/" + testSHA256 + ".txt"
	lastRedirected := time.Date(2026, 6, 28, 12, 1, 0, 0, time.UTC)
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: key, Size: 12, ContentType: "text/plain", Status: "ready"}
	metadata.aliases["mine.txt"] = ShareAlias{Slug: "mine.txt", ObjectSHA256: testSHA256, ObjectKey: key, Owner: "user-1", Visibility: "public", Status: AliasStatusReady, RedirectCount: 7, LastRedirectedAt: lastRedirected}
	store := &fakeStore{objects: map[string][]byte{key: []byte("hello")}}
	server := NewServer(testConfig(t), authenticatedFakeAuth("user-1"), store, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodDelete, "/api/shares/mine.txt", nil)
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	alias := metadata.aliases["mine.txt"]
	if alias.Visibility != "deleted" || alias.RedirectCount != 7 || !alias.LastRedirectedAt.Equal(lastRedirected) {
		t.Fatalf("alias = %#v", alias)
	}
	if len(store.deleted) != 1 || store.deleted[0] != key {
		t.Fatalf("deleted = %#v", store.deleted)
	}
	if metadata.objects[testSHA256].Status != "deleted" {
		t.Fatalf("object = %#v", metadata.objects[testSHA256])
	}
}

func TestDeleteShareRemovesThumbnailWithUnreferencedObject(t *testing.T) {
	metadata := newMemoryMetadata()
	key := "01/" + testSHA256 + ".mp4"
	thumbnailKey := "01/" + testSHA256 + ".jpg"
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: key, Size: 12, ContentType: "video/mp4", Status: "ready", ThumbnailKey: thumbnailKey}
	metadata.aliases["mine.mp4"] = ShareAlias{Slug: "mine.mp4", ObjectSHA256: testSHA256, ObjectKey: key, Owner: "user-1", Visibility: "public", Status: AliasStatusReady, ThumbnailKey: thumbnailKey}
	store := &fakeStore{objects: map[string][]byte{key: []byte("video"), thumbnailKey: []byte("jpeg")}}
	server := NewServer(testConfig(t), authenticatedFakeAuth("user-1"), store, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodDelete, "/api/shares/mine.mp4", nil)
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(store.deleted) != 2 || store.deleted[0] != key || store.deleted[1] != thumbnailKey {
		t.Fatalf("deleted = %#v", store.deleted)
	}
}

func TestDeleteShareKeepsB2ObjectStillReferenced(t *testing.T) {
	metadata := newMemoryMetadata()
	key := "01/" + testSHA256 + ".txt"
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: key, Size: 12, ContentType: "text/plain", Status: "ready"}
	metadata.aliases["mine.txt"] = ShareAlias{Slug: "mine.txt", ObjectSHA256: testSHA256, ObjectKey: key, Owner: "user-1", Visibility: "public", Status: AliasStatusReady}
	metadata.aliases["also-mine.txt"] = ShareAlias{Slug: "also-mine.txt", ObjectSHA256: testSHA256, ObjectKey: key, Owner: "user-1", Visibility: "public", Status: AliasStatusReady}
	store := &fakeStore{objects: map[string][]byte{key: []byte("hello")}}
	server := NewServer(testConfig(t), authenticatedFakeAuth("user-1"), store, metadata, slog.Default())
	request := httptest.NewRequest(http.MethodDelete, "/api/shares/mine.txt", nil)
	setCSRF(request)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(store.deleted) != 0 {
		t.Fatalf("deleted = %#v", store.deleted)
	}
	if metadata.objects[testSHA256].Status == "deleted" {
		t.Fatalf("object = %#v", metadata.objects[testSHA256])
	}
}

func TestShareTargetPostRejectsServerSideByteFallback(t *testing.T) {
	server := NewServer(testConfig(t), fakeAuth{err: ErrUnauthorized}, &fakeStore{}, newMemoryMetadata(), slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/share-target", strings.NewReader("body"))
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}
