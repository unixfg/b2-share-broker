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
	cfg    Config
	auth   Authenticator
	store  ObjectStore
	logger *slog.Logger
	mux    *http.ServeMux
}

type createUploadRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

type createUploadResponse struct {
	UploadURL       string            `json:"uploadUrl"`
	RequiredHeaders map[string]string `json:"requiredHeaders"`
	ObjectKey       string            `json:"objectKey"`
	UploadToken     string            `json:"uploadToken"`
	PublicURL       string            `json:"publicUrl"`
}

type completeUploadRequest struct {
	UploadToken string `json:"uploadToken"`
}

type completeUploadResponse struct {
	ObjectKey string `json:"objectKey"`
	PublicURL string `json:"publicUrl"`
	Verified  bool   `json:"verified"`
	Size      int64  `json:"size,omitempty"`
	ETag      string `json:"etag,omitempty"`
}

func NewServer(cfg Config, auth Authenticator, store ObjectStore, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{
		cfg:    cfg,
		auth:   auth,
		store:  store,
		logger: logger,
		mux:    http.NewServeMux(),
	}
	server.mux.HandleFunc("/healthz", server.handleHealthz)
	server.mux.HandleFunc("/s/", server.handlePublicShare)
	server.mux.HandleFunc("/api/uploads", server.handleCreateUpload)
	server.mux.HandleFunc("/api/uploads/complete", server.handleCompleteUpload)
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
	objectKey, ok := objectKeyFromSharePath(r.URL.EscapedPath(), s.cfg.ObjectPrefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, PublicURL(s.cfg.B2PublicBaseURL, objectKey), http.StatusFound)
}

func (s *Server) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	principal, err := s.auth.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
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
	if request.Size <= 0 {
		writeJSONError(w, http.StatusBadRequest, "size must be positive")
		return
	}
	if request.Size > s.cfg.MaxUploadBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "file is larger than the configured maximum")
		return
	}

	now := s.cfg.Clock().UTC()
	objectKey, err := GenerateObjectKey(now, s.cfg.Entropy, s.cfg.ObjectPrefix, request.Filename)
	if err != nil {
		s.logger.Error("failed to generate object key", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to generate object key")
		return
	}
	presigned, err := s.store.PresignPutObject(r.Context(), objectKey, request.ContentType, request.Size, s.cfg.PresignTTL)
	if err != nil {
		s.logger.Error("failed to presign upload", "error", err)
		writeJSONError(w, http.StatusBadGateway, "failed to create upload target")
		return
	}
	uploadToken, err := SignUploadToken(s.cfg.UploadTokenKey, uploadTokenPayload{
		ObjectKey:   objectKey,
		ContentType: request.ContentType,
		Size:        request.Size,
		Subject:     principal.Subject,
		ExpiresAt:   now.Add(s.cfg.UploadTokenTTL).Unix(),
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
		PublicURL:       ShareURL(s.cfg.PublicBaseURL, objectKey),
	})
}

func (s *Server) handleCompleteUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	principal, err := s.auth.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
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
	if payload.Subject != principal.Subject {
		writeJSONError(w, http.StatusForbidden, "upload token does not belong to this principal")
		return
	}

	response := completeUploadResponse{
		ObjectKey: payload.ObjectKey,
		PublicURL: ShareURL(s.cfg.PublicBaseURL, payload.ObjectKey),
	}
	metadata, err := s.store.HeadObject(r.Context(), payload.ObjectKey)
	if err != nil {
		s.logger.Warn("uploaded object was not verified by HEAD", "objectKey", payload.ObjectKey, "error", err)
		writeJSON(w, http.StatusOK, response)
		return
	}
	response.Verified = true
	response.Size = metadata.ContentLength
	response.ETag = metadata.ETag
	writeJSON(w, http.StatusOK, response)
}

func objectKeyFromSharePath(escapedPath, objectPrefix string) (string, bool) {
	const sharePrefix = "/s/"
	if !strings.HasPrefix(escapedPath, sharePrefix) {
		return "", false
	}
	escapedObjectKey := strings.TrimPrefix(escapedPath, sharePrefix)
	if escapedObjectKey == "" {
		return "", false
	}
	segments := strings.Split(escapedObjectKey, "/")
	decoded := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" {
			return "", false
		}
		value, err := url.PathUnescape(segment)
		if err != nil || value == "" || strings.Contains(value, "/") {
			return "", false
		}
		decoded = append(decoded, value)
	}
	objectKey := strings.Join(decoded, "/")
	prefix := strings.Trim(objectPrefix, "/")
	if objectKey == prefix || strings.HasPrefix(objectKey, prefix+"/") {
		return objectKey, true
	}
	return "", false
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
