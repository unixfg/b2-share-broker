package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	MediaURL        string `json:"mediaUrl,omitempty"`
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
	if r.Method == http.MethodOptions {
		s.handlePublicSharePreflight(w, r)
		return
	}
	s.applyPublicShareCORS(w, r)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
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
		writeShareRedirect(w, PublicURL(s.cfg.B2PublicBaseURL, alias.ObjectKey), alias.ContentType)
	default:
		writeShareStatusPage(w, r, http.StatusAccepted, "Processing", "This share is still being prepared.")
	}
}

func (s *Server) handlePublicSharePreflight(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "GET, HEAD, OPTIONS")
	if s.applyPublicShareCORS(w, r) {
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Range")
		w.Header().Set("Access-Control-Max-Age", "3600")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) applyPublicShareCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	w.Header().Add("Vary", "Origin")
	w.Header().Add("Vary", "Access-Control-Request-Method")
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	for _, allowed := range s.cfg.PublicShareCORSAllowedOrigins {
		if origin != allowed {
			continue
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", "Accept-Ranges, Content-Length, Content-Range, Content-Type, ETag, Last-Modified, Location")
		return true
	}
	return false
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

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeJSONError(w, http.StatusBadRequest, "multipart/form-data file upload is required")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+16<<20)
	reader, err := r.MultipartReader()
	if err != nil {
		s.writeMultipartError(w, r, err)
		return
	}

	jobID, err := NewProcessingJobID()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create upload job")
		return
	}

	upload, err := s.stageMultipartUpload(r, reader, jobID)
	if err != nil {
		s.writeUploadStagingError(w, r, err, "")
		return
	}

	slug, err := GenerateRandomAliasSlug(upload.filename, upload.finalExtension)
	if err != nil {
		_ = os.Remove(upload.stagingPath)
		writeJSONError(w, http.StatusInternalServerError, "failed to create share slug")
		return
	}

	job := ProcessingJob{
		ID:              jobID,
		Owner:           authenticated.Principal.Subject,
		AliasSlug:       slug,
		StagingPath:     upload.stagingPath,
		Profile:         upload.profile,
		Status:          ProcessingStatusQueued,
		DisplayFilename: upload.filename,
		SourceSize:      upload.size,
		SourceType:      upload.contentType,
	}
	created, err := s.metadata.CreateIngestJob(r.Context(), job, ShareAlias{
		Slug:            slug,
		Owner:           authenticated.Principal.Subject,
		DisplayFilename: upload.filename,
		Visibility:      "public",
		Status:          AliasStatusPending,
	})
	if err != nil {
		_ = os.Remove(upload.stagingPath)
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
	aliases, err := s.metadata.ListAliases(
		r.Context(),
		authenticated.Principal.Subject,
		r.URL.Query().Get("q"),
		parseShareLimit(r.URL.Query().Get("limit")),
	)
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

func parseShareLimit(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 50
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 {
		return 50
	}
	if limit > 100 {
		return 100
	}
	return limit
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
var errUploadMissingFile = errors.New("upload file required")
var errUploadMultipleFiles = errors.New("multiple upload files")
var errUploadCustomAlias = errors.New("custom upload alias")
var errMultipartRead = errors.New("multipart read failed")

type stagedMultipartUpload struct {
	filename       string
	contentType    string
	finalExtension string
	profile        string
	stagingPath    string
	size           int64
}

func (s *Server) stageMultipartUpload(r *http.Request, reader *multipart.Reader, jobID string) (stagedMultipartUpload, error) {
	var upload stagedMultipartUpload
	fileSeen := false

	cleanup := func() {
		if upload.stagingPath != "" {
			_ = os.Remove(upload.stagingPath)
		}
	}

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return stagedMultipartUpload{}, fmt.Errorf("%w: %w", errMultipartRead, err)
		}

		switch part.FormName() {
		case "file":
			if fileSeen {
				_ = part.Close()
				cleanup()
				return stagedMultipartUpload{}, errUploadMultipleFiles
			}
			fileSeen = true
			filename := SanitizeFilename(part.FileName())
			contentType := strings.TrimSpace(part.Header.Get("Content-Type"))
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
			stagingPath, size, err := s.stageUpload(jobID, finalExtension, part)
			closeErr := part.Close()
			if err != nil {
				cleanup()
				return stagedMultipartUpload{}, err
			}
			if closeErr != nil {
				_ = os.Remove(stagingPath)
				cleanup()
				return stagedMultipartUpload{}, fmt.Errorf("%w: %w", errMultipartRead, closeErr)
			}
			upload = stagedMultipartUpload{
				filename:       filename,
				contentType:    contentType,
				finalExtension: finalExtension,
				profile:        profile,
				stagingPath:    stagingPath,
				size:           size,
			}
		case "alias":
			value, err := io.ReadAll(io.LimitReader(part, 4097))
			_ = part.Close()
			if err != nil {
				cleanup()
				return stagedMultipartUpload{}, fmt.Errorf("%w: %w", errMultipartRead, err)
			}
			if len(value) > 4096 || strings.TrimSpace(string(value)) != "" {
				cleanup()
				return stagedMultipartUpload{}, errUploadCustomAlias
			}
		default:
			if _, err := io.Copy(io.Discard, part); err != nil {
				_ = part.Close()
				cleanup()
				return stagedMultipartUpload{}, fmt.Errorf("%w: %w", errMultipartRead, err)
			}
			_ = part.Close()
		}
	}

	if !fileSeen {
		return stagedMultipartUpload{}, errUploadMissingFile
	}
	if upload.stagingPath == "" {
		return stagedMultipartUpload{}, errUploadMissingFile
	}
	return upload, nil
}

func (s *Server) writeUploadStagingError(w http.ResponseWriter, r *http.Request, err error, stagingPath string) {
	if stagingPath != "" {
		_ = os.Remove(stagingPath)
	}
	switch {
	case errors.Is(err, errUploadMissingFile):
		writeJSONError(w, http.StatusBadRequest, "file is required")
	case errors.Is(err, errUploadMultipleFiles):
		writeJSONError(w, http.StatusBadRequest, "share one file at a time")
	case errors.Is(err, errUploadCustomAlias):
		writeJSONError(w, http.StatusBadRequest, "custom aliases are not supported")
	case errors.Is(err, errUploadTooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "file is larger than the configured maximum")
	case errors.Is(err, errMultipartRead):
		s.writeMultipartError(w, r, err)
	default:
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) || strings.Contains(err.Error(), "request body too large") {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "file is larger than the configured maximum")
			return
		}
		s.logger.Error("failed to stage upload", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to stage upload")
	}
}

func (s *Server) writeMultipartError(w http.ResponseWriter, r *http.Request, err error) {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) || strings.Contains(err.Error(), "request body too large") {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "file is larger than the configured maximum")
		return
	}
	s.logger.Warn("failed to read multipart upload", "content_type", r.Header.Get("Content-Type"), "error", err)
	writeJSONError(w, http.StatusBadRequest, "multipart file upload is invalid")
}

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

func writeShareRedirect(w http.ResponseWriter, location, contentType string) {
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Location", location)
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusFound)
}

func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(value)
}

func uploadStatusResponseFromJob(cfg Config, job ProcessingJob) uploadStatusResponse {
	mediaURL := ""
	if job.TargetObjectKey != "" {
		mediaURL = PublicURL(cfg.B2PublicBaseURL, job.TargetObjectKey)
	}
	return uploadStatusResponse{
		JobID:           job.ID,
		Status:          job.Status,
		Profile:         job.Profile,
		Slug:            job.AliasSlug,
		ShareURL:        ShareURL(cfg.PublicBaseURL, job.AliasSlug),
		MediaURL:        mediaURL,
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
