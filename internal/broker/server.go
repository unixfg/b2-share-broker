package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Server struct {
	cfg      Config
	auth     Authenticator
	login    *OIDCLogin
	store    ObjectStore
	metadata MetadataStore
	logger   *slog.Logger
	mux      *http.ServeMux
}

type createUploadResponse struct {
	ShareURL string `json:"shareUrl"`
	Slug     string `json:"slug"`
	JobID    string `json:"jobId"`
	Status   string `json:"status"`
}

type uploadStatusResponse struct {
	JobID           string `json:"jobId"`
	Status          string `json:"status"`
	Profile         string `json:"profile"`
	Slug            string `json:"slug"`
	ShareURL        string `json:"shareUrl"`
	TargetSHA256    string `json:"targetSha256,omitempty"`
	TargetObjectKey string `json:"targetObjectKey,omitempty"`
	Error           string `json:"error,omitempty"`
}

type listSharesResponse struct {
	Shares []ShareAlias `json:"shares"`
}

func NewServer(cfg Config, auth Authenticator, store ObjectStore, metadata MetadataStore, logger *slog.Logger) *Server {
	return NewServerWithLogin(cfg, auth, nil, store, metadata, logger)
}

func NewServerWithLogin(cfg Config, auth Authenticator, login *OIDCLogin, store ObjectStore, metadata MetadataStore, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{
		cfg:      cfg,
		auth:     auth,
		login:    login,
		store:    store,
		metadata: metadata,
		logger:   logger,
		mux:      http.NewServeMux(),
	}
	server.mux.HandleFunc("/healthz", server.handleHealthz)
	server.mux.HandleFunc("/s/", server.handlePublicShare)
	server.mux.HandleFunc("/auth/login", server.handleLogin)
	server.mux.HandleFunc("/auth/callback", server.handleCallback)
	server.mux.HandleFunc("/auth/logout", server.handleLogout)
	server.mux.HandleFunc("/api/session", server.handleSession)
	server.mux.HandleFunc("/api/uploads", server.handleCreateUpload)
	server.mux.HandleFunc("/api/uploads/", server.handleUploadStatus)
	server.mux.HandleFunc("/api/shares", server.handleListShares)
	server.mux.HandleFunc("/api/shares/", server.handleShare)
	server.mux.HandleFunc("/share-target", server.handleShareTargetFallback)
	server.registerWebRoutes()
	return server
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte("ok\n"))
	}
}

func (s *Server) handlePublicShare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	slug, ok := shareSlugFromPath(r.URL.EscapedPath())
	if !ok {
		http.NotFound(w, r)
		return
	}
	alias, found, err := s.metadata.GetAlias(r.Context(), slug)
	if err != nil {
		s.logger.Error("failed to resolve share alias", "slug", slug, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to resolve share")
		return
	}
	if !found || alias.Visibility != "public" {
		http.NotFound(w, r)
		return
	}
	switch alias.Status {
	case AliasStatusPending:
		writeShareStatusPage(w, r, http.StatusAccepted, "Processing", "This share is still being prepared.")
	case AliasStatusFailed:
		writeShareStatusPage(w, r, http.StatusServiceUnavailable, "Unavailable", "This share could not be prepared.")
	case AliasStatusReady, "":
		if alias.ObjectKey == "" {
			writeShareStatusPage(w, r, http.StatusAccepted, "Processing", "This share is still being prepared.")
			return
		}
		if err := s.metadata.RecordAliasRedirect(r.Context(), slug); err != nil {
			s.logger.Warn("failed to record share redirect", "slug", slug, "error", err)
		}
		http.Redirect(w, r, PublicURL(s.cfg.B2PublicBaseURL, alias.ObjectKey), http.StatusFound)
	default:
		writeShareStatusPage(w, r, http.StatusAccepted, "Processing", "This share is still being prepared.")
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.login == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "login is not configured")
		return
	}
	s.login.Start(w, r)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.login == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "login is not configured")
		return
	}
	s.login.Complete(w, r)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.login == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "login is not configured")
		return
	}
	s.login.Logout(w, r)
}

type sessionResponse struct {
	Authenticated bool      `json:"authenticated"`
	User          Principal `json:"user,omitempty"`
	CSRFToken     string    `json:"csrfToken,omitempty"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authenticated, err := s.auth.Authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusOK, sessionResponse{Authenticated: false})
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{
		Authenticated: true,
		User:          authenticated.Principal,
		CSRFToken:     authenticated.CSRFToken,
	})
}

func (s *Server) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authenticated, err := s.auth.Authenticate(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if err := requireCSRF(authenticated, r); err != nil {
		writeAuthError(w, err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+16<<20)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeJSONError(w, http.StatusBadRequest, "multipart file upload is required")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer file.Close()

	filename := SanitizeFilename(header.Filename)
	contentType := strings.TrimSpace(header.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	finalExtension := ExtensionFor(filename, contentType)
	profile := ProcessingProfileUploadFinalize
	if looksLikeVideo(filename, contentType) {
		finalExtension = ".mp4"
		contentType = normalizedContentType(contentType)
		profile = ProcessingProfileMP4Web
	}
	if strings.TrimSpace(r.FormValue("alias")) != "" {
		writeJSONError(w, http.StatusBadRequest, "custom aliases are not supported")
		return
	}
	slug, err := GenerateRandomAliasSlug(filename, finalExtension)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create share slug")
		return
	}
	jobID, err := NewProcessingJobID()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create upload job")
		return
	}
	stagingPath, size, err := s.stageUpload(jobID, finalExtension, file)
	if err != nil {
		if errors.Is(err, errUploadTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "file is larger than the configured maximum")
			return
		}
		s.logger.Error("failed to stage upload", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to stage upload")
		return
	}

	job := ProcessingJob{
		ID:              jobID,
		Owner:           authenticated.Principal.Subject,
		AliasSlug:       slug,
		StagingPath:     stagingPath,
		Profile:         profile,
		Status:          ProcessingStatusQueued,
		DisplayFilename: filename,
		SourceSize:      size,
		SourceType:      contentType,
	}
	created, err := s.metadata.CreateIngestJob(r.Context(), job, ShareAlias{
		Slug:            slug,
		Owner:           authenticated.Principal.Subject,
		DisplayFilename: filename,
		Visibility:      "public",
		Status:          AliasStatusPending,
	})
	if err != nil {
		_ = os.Remove(stagingPath)
		if errors.Is(err, ErrAliasConflict) {
			writeJSONError(w, http.StatusConflict, "share alias is already in use")
			return
		}
		s.logger.Error("failed to create ingest job", "slug", slug, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to create upload job")
		return
	}
	writeJSON(w, http.StatusAccepted, createUploadResponse{
		ShareURL: ShareURL(s.cfg.PublicBaseURL, slug),
		Slug:     slug,
		JobID:    created.ID,
		Status:   created.Status,
	})
}

func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authenticated, err := s.auth.Authenticate(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	jobID, ok := slugFromEscapedPath(r.URL.EscapedPath(), "/api/uploads/")
	if !ok || jobID == "complete" {
		http.NotFound(w, r)
		return
	}
	job, found, err := s.metadata.GetProcessingJob(r.Context(), jobID, authenticated.Principal.Subject)
	if err != nil {
		s.logger.Error("failed to get upload job", "jobID", jobID, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to get upload job")
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, uploadStatusResponseFromJob(s.cfg, job))
}

func (s *Server) handleListShares(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authenticated, err := s.auth.Authenticate(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	aliases, err := s.metadata.ListAliases(r.Context(), authenticated.Principal.Subject, 50)
	if err != nil {
		s.logger.Error("failed to list shares", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to list shares")
		return
	}
	for index := range aliases {
		aliases[index].PublicURL = ShareURL(s.cfg.PublicBaseURL, aliases[index].Slug)
		if aliases[index].ObjectKey != "" {
			aliases[index].B2URL = PublicURL(s.cfg.B2PublicBaseURL, aliases[index].ObjectKey)
		}
	}
	writeJSON(w, http.StatusOK, listSharesResponse{Shares: aliases})
}

func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authenticated, err := s.auth.Authenticate(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if err := requireCSRF(authenticated, r); err != nil {
		writeAuthError(w, err)
		return
	}
	slug, ok := slugFromEscapedPath(r.URL.EscapedPath(), "/api/shares/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	deleted, found, err := s.metadata.DeleteAlias(r.Context(), slug, authenticated.Principal.Subject)
	if err != nil {
		s.logger.Error("failed to delete share alias", "slug", slug, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to delete share")
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	for _, stagingPath := range deleted.StagingPaths {
		if err := os.Remove(stagingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("failed to remove staged upload", "path", stagingPath, "error", err)
		}
	}
	if deleted.ObjectKey != "" {
		if err := s.store.DeleteObject(r.Context(), deleted.ObjectKey); err != nil {
			s.logger.Warn("failed to delete unreferenced B2 object", "objectKey", deleted.ObjectKey, "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleShareTargetFallback(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		http.Redirect(w, r, "/share", http.StatusFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, HEAD, POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSONError(w, http.StatusBadRequest, "install the web app before using it as a share target")
}

var errUploadTooLarge = errors.New("upload too large")

func (s *Server) stageUpload(jobID, extension string, reader io.Reader) (string, int64, error) {
	if err := os.MkdirAll(s.cfg.StagingDir, 0o700); err != nil {
		return "", 0, err
	}
	name := safeFilename(jobID) + extension + ".upload"
	path := filepath.Join(s.cfg.StagingDir, name)
	tempPath := path + ".tmp"
	output, err := os.OpenFile(tempPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	limited := &limitedWriter{Writer: output, Limit: s.cfg.MaxUploadBytes}
	_, copyErr := io.Copy(limited, reader)
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(tempPath)
		return "", 0, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tempPath)
		return "", 0, closeErr
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return "", 0, err
	}
	return path, limited.Written, nil
}

type limitedWriter struct {
	Writer  io.Writer
	Limit   int64
	Written int64
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if w.Written+int64(len(data)) > w.Limit {
		return 0, errUploadTooLarge
	}
	n, err := w.Writer.Write(data)
	w.Written += int64(n)
	return n, err
}

func shareSlugFromPath(escapedPath string) (string, bool) {
	return slugFromEscapedPath(escapedPath, "/s/")
}

func slugFromEscapedPath(escapedPath, prefix string) (string, bool) {
	if !strings.HasPrefix(escapedPath, prefix) {
		return "", false
	}
	escapedSlug := strings.TrimPrefix(escapedPath, prefix)
	if escapedSlug == "" || strings.Contains(escapedSlug, "/") {
		return "", false
	}
	slug, err := url.PathUnescape(escapedSlug)
	if err != nil || slug == "" || strings.Contains(slug, "/") {
		return "", false
	}
	return slug, true
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "body must contain exactly one JSON object")
		return errors.New("extra JSON data")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]string{"error": message})
}

func writeShareStatusPage(w http.ResponseWriter, r *http.Request, statusCode int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(statusCode)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>%s</title><body><main><h1>%s</h1><p>%s</p></main></body>", htmlEscape(title), htmlEscape(title), htmlEscape(message))
}

func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(value)
}

func uploadStatusResponseFromJob(cfg Config, job ProcessingJob) uploadStatusResponse {
	return uploadStatusResponse{
		JobID:           job.ID,
		Status:          job.Status,
		Profile:         job.Profile,
		Slug:            job.AliasSlug,
		ShareURL:        ShareURL(cfg.PublicBaseURL, job.AliasSlug),
		TargetSHA256:    job.TargetSHA256,
		TargetObjectKey: job.TargetObjectKey,
		Error:           job.Error,
	}
}

func looksLikeVideo(filename, contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	name := strings.ToLower(filename)
	return strings.HasPrefix(contentType, "video/") ||
		strings.HasSuffix(name, ".mp4") ||
		strings.HasSuffix(name, ".m4v") ||
		strings.HasSuffix(name, ".mov") ||
		strings.HasSuffix(name, ".mkv") ||
		strings.HasSuffix(name, ".webm") ||
		strings.HasSuffix(name, ".avi")
}

func normalizedContentType(contentType string) string {
	if looksLikeVideo("", contentType) {
		return contentType
	}
	return "application/octet-stream"
}
