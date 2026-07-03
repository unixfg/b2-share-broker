package broker

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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

type createUploadRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Alias       string `json:"alias"`
}

type createUploadResponse struct {
	UploadURL       string            `json:"uploadUrl,omitempty"`
	RequiredHeaders map[string]string `json:"requiredHeaders,omitempty"`
	ObjectKey       string            `json:"objectKey"`
	UploadToken     string            `json:"uploadToken,omitempty"`
	PublicURL       string            `json:"publicUrl"`
	B2URL           string            `json:"b2Url"`
	AlreadyUploaded bool              `json:"alreadyUploaded"`
}

type completeUploadRequest struct {
	UploadToken string `json:"uploadToken"`
}

type completeUploadResponse struct {
	ObjectKey string `json:"objectKey"`
	SHA256    string `json:"sha256"`
	PublicURL string `json:"publicUrl"`
	B2URL     string `json:"b2Url"`
	Verified  bool   `json:"verified"`
	Size      int64  `json:"size,omitempty"`
	ETag      string `json:"etag,omitempty"`
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
	server.mux.HandleFunc("/api/uploads/complete", server.handleCompleteUpload)
	server.mux.HandleFunc("/api/shares", server.handleListShares)
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
	if err := s.metadata.RecordAliasRedirect(r.Context(), slug); err != nil {
		s.logger.Warn("failed to record share redirect", "slug", slug, "error", err)
	}
	http.Redirect(w, r, PublicURL(s.cfg.B2PublicBaseURL, alias.ObjectKey), http.StatusFound)
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

	var request createUploadRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		return
	}
	request.Filename = strings.TrimSpace(request.Filename)
	request.ContentType = strings.TrimSpace(request.ContentType)
	if request.ContentType == "" {
		request.ContentType = "application/octet-stream"
	}
	if request.Filename == "" {
		writeJSONError(w, http.StatusBadRequest, "filename is required")
		return
	}
	sha256Hex, sha256Bytes, err := NormalizeSHA256(request.SHA256)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "sha256 is required")
		return
	}
	if request.Size <= 0 {
		writeJSONError(w, http.StatusBadRequest, "size must be positive")
		return
	}
	if request.Size > s.cfg.MaxUploadBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "file is larger than the configured maximum")
		return
	}

	now := s.cfg.Clock().UTC()
	extension := ExtensionFor(request.Filename, request.ContentType)
	objectKey := GenerateObjectKey(s.cfg.ObjectPrefix, sha256Hex, extension)
	displayFilename := SanitizeFilename(request.Filename)
	aliasSlug := GenerateAliasSlug(s.cfg.AliasHMACKey, sha256Bytes, extension)
	if strings.TrimSpace(request.Alias) != "" {
		aliasSlug = NormalizeManualAlias(request.Alias, extension)
	}
	publicURL := ShareURL(s.cfg.PublicBaseURL, aliasSlug)
	b2URL := PublicURL(s.cfg.B2PublicBaseURL, objectKey)

	if object, found, err := s.metadata.GetObject(r.Context(), sha256Hex); err != nil {
		s.logger.Error("failed to look up object metadata", "sha256", sha256Hex, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to look up object")
		return
	} else if found {
		if err := s.metadata.UpsertAlias(r.Context(), ShareAlias{
			Slug:            aliasSlug,
			ObjectSHA256:    object.SHA256,
			ObjectKey:       object.ObjectKey,
			Owner:           authenticated.Principal.Subject,
			DisplayFilename: displayFilename,
			Visibility:      "public",
		}); err != nil {
			s.logger.Error("failed to record share alias", "slug", aliasSlug, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to record share alias")
			return
		}
		writeJSON(w, http.StatusOK, createUploadResponse{
			ObjectKey:       object.ObjectKey,
			PublicURL:       publicURL,
			B2URL:           PublicURL(s.cfg.B2PublicBaseURL, object.ObjectKey),
			AlreadyUploaded: true,
		})
		return
	}

	presigned, err := s.store.PresignPutObject(r.Context(), objectKey, request.ContentType, request.Size, s.cfg.PresignTTL)
	if err != nil {
		s.logger.Error("failed to presign upload", "error", err)
		writeJSONError(w, http.StatusBadGateway, "failed to create upload target")
		return
	}
	uploadToken, err := SignUploadToken(s.cfg.UploadTokenKey, uploadTokenPayload{
		ObjectKey:       objectKey,
		SHA256:          sha256Hex,
		AliasSlug:       aliasSlug,
		DisplayFilename: displayFilename,
		ContentType:     request.ContentType,
		Extension:       extension,
		Size:            request.Size,
		Subject:         authenticated.Principal.Subject,
		ExpiresAt:       now.Add(s.cfg.UploadTokenTTL).Unix(),
	})
	if err != nil {
		s.logger.Error("failed to sign upload token", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to create upload token")
		return
	}

	writeJSON(w, http.StatusCreated, createUploadResponse{
		UploadURL:       presigned.URL,
		RequiredHeaders: requiredHeaders(presigned.Header),
		ObjectKey:       objectKey,
		UploadToken:     uploadToken,
		PublicURL:       publicURL,
		B2URL:           b2URL,
	})
}

func (s *Server) handleCompleteUpload(w http.ResponseWriter, r *http.Request) {
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

	var request completeUploadRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		return
	}
	payload, err := VerifyUploadToken(s.cfg.UploadTokenKey, request.UploadToken, s.cfg.Clock().UTC())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid upload token")
		return
	}
	if payload.Subject != authenticated.Principal.Subject {
		writeJSONError(w, http.StatusForbidden, "upload token does not belong to this principal")
		return
	}

	response := completeUploadResponse{
		ObjectKey: payload.ObjectKey,
		SHA256:    payload.SHA256,
		PublicURL: ShareURL(s.cfg.PublicBaseURL, payload.AliasSlug),
		B2URL:     PublicURL(s.cfg.B2PublicBaseURL, payload.ObjectKey),
	}
	metadata, err := s.store.HeadObject(r.Context(), payload.ObjectKey)
	if err != nil {
		s.logger.Warn("uploaded object was not verified by HEAD", "objectKey", payload.ObjectKey, "error", err)
		writeJSONError(w, http.StatusBadGateway, "failed to verify uploaded object")
		return
	}
	if metadata.ContentLength > 0 && metadata.ContentLength != payload.Size {
		writeJSONError(w, http.StatusBadGateway, "uploaded object size did not match")
		return
	}
	contentType := payload.ContentType
	if strings.TrimSpace(metadata.ContentType) != "" {
		contentType = metadata.ContentType
	}
	if err := s.metadata.UpsertObjectAndAlias(r.Context(), StoredObject{
		SHA256:        payload.SHA256,
		ObjectKey:     payload.ObjectKey,
		Size:          payload.Size,
		ContentType:   contentType,
		Extension:     payload.Extension,
		FirstFilename: payload.DisplayFilename,
		Uploader:      authenticated.Principal.Subject,
	}, ShareAlias{
		Slug:            payload.AliasSlug,
		ObjectSHA256:    payload.SHA256,
		ObjectKey:       payload.ObjectKey,
		Owner:           authenticated.Principal.Subject,
		DisplayFilename: payload.DisplayFilename,
		Visibility:      "public",
	}); err != nil {
		s.logger.Error("failed to record uploaded object", "objectKey", payload.ObjectKey, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to record uploaded object")
		return
	}
	response.Verified = true
	response.Size = metadata.ContentLength
	response.ETag = metadata.ETag
	writeJSON(w, http.StatusOK, response)
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
		aliases[index].B2URL = PublicURL(s.cfg.B2PublicBaseURL, aliases[index].ObjectKey)
	}
	writeJSON(w, http.StatusOK, listSharesResponse{Shares: aliases})
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

func shareSlugFromPath(escapedPath string) (string, bool) {
	const sharePrefix = "/s/"
	if !strings.HasPrefix(escapedPath, sharePrefix) {
		return "", false
	}
	escapedSlug := strings.TrimPrefix(escapedPath, sharePrefix)
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

func requiredHeaders(header http.Header) map[string]string {
	result := map[string]string{}
	for name, values := range header {
		if len(values) == 0 || strings.EqualFold(name, "host") {
			continue
		}
		result[http.CanonicalHeaderKey(name)] = values[0]
	}
	return result
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]string{"error": message})
}
