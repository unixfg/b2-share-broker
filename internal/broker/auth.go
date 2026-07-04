package broker

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookieName    = "b2_share_session"
	oauthStateCookieName = "b2_share_oauth"
	csrfHeaderName       = "X-CSRF-Token"
	oauthStateTTL        = 10 * time.Minute
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)

type Principal struct {
	Subject           string   `json:"sub"`
	Email             string   `json:"email,omitempty"`
	PreferredUsername string   `json:"preferred_username,omitempty"`
	Roles             []string `json:"roles,omitempty"`
}

type AuthenticatedRequest struct {
	Principal    Principal
	CSRFToken    string
	RequiresCSRF bool
}

type Authenticator interface {
	Authenticate(*http.Request) (AuthenticatedRequest, error)
}

type SessionAuthenticator struct {
	sessions *SessionManager
}

func NewSessionAuthenticator(sessions *SessionManager) SessionAuthenticator {
	return SessionAuthenticator{sessions: sessions}
}

func (a SessionAuthenticator) Authenticate(r *http.Request) (AuthenticatedRequest, error) {
	if bearerToken(r) != "" {
		return AuthenticatedRequest{}, ErrUnauthorized
	}
	session, err := a.sessions.Read(r)
	if err != nil {
		return AuthenticatedRequest{}, err
	}
	return AuthenticatedRequest{
		Principal:    session.Principal,
		CSRFToken:    session.CSRFToken,
		RequiresCSRF: true,
	}, nil
}

type CombinedAuthenticator struct {
	session Authenticator
	bearer  Authenticator
}

func NewCombinedAuthenticator(session, bearer Authenticator) CombinedAuthenticator {
	return CombinedAuthenticator{session: session, bearer: bearer}
}

func (a CombinedAuthenticator) Authenticate(r *http.Request) (AuthenticatedRequest, error) {
	if bearerToken(r) != "" {
		return a.bearer.Authenticate(r)
	}
	return a.session.Authenticate(r)
}

type BearerAuthenticator struct {
	verifier *oidc.IDTokenVerifier
	roles    roleAuthorizer
	clientID string
}

func NewBearerAuthenticator(ctx context.Context, cfg Config) (*BearerAuthenticator, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	return &BearerAuthenticator{
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.OIDCAudience}),
		roles:    newRoleAuthorizer(cfg.RequiredRoles),
		clientID: cfg.OIDCClientID,
	}, nil
}

func (a *BearerAuthenticator) Authenticate(r *http.Request) (AuthenticatedRequest, error) {
	raw := bearerToken(r)
	if raw == "" {
		return AuthenticatedRequest{}, ErrUnauthorized
	}
	token, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		return AuthenticatedRequest{}, ErrUnauthorized
	}
	var claims keycloakClaims
	if err := token.Claims(&claims); err != nil {
		return AuthenticatedRequest{}, ErrUnauthorized
	}
	principal := principalFromClaims(token.Subject, claims, a.clientID)
	if !a.roles.Allows(claims, a.clientID) {
		return AuthenticatedRequest{}, ErrForbidden
	}
	return AuthenticatedRequest{Principal: principal}, nil
}

type Session struct {
	Principal Principal
	CSRFToken string
	ExpiresAt int64
}

type SessionManager struct {
	key    []byte
	ttl    time.Duration
	clock  func() time.Time
	secure bool
}

func NewSessionManager(cfg Config) *SessionManager {
	return &SessionManager{
		key:    cfg.SessionAuthKey,
		ttl:    cfg.SessionTTL,
		clock:  cfg.Clock,
		secure: strings.HasPrefix(cfg.PublicBaseURL, "https://"),
	}
}

func (m *SessionManager) Create(w http.ResponseWriter, principal Principal) (Session, error) {
	csrfToken, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	session := Session{
		Principal: principal,
		CSRFToken: csrfToken,
		ExpiresAt: m.clock().UTC().Add(m.ttl).Unix(),
	}
	token, err := signJSON(m.key, session)
	if err != nil {
		return Session{}, err
	}
	http.SetCookie(w, m.cookie(sessionCookieName, token, int(m.ttl.Seconds())))
	return session, nil
}

func (m *SessionManager) Read(r *http.Request) (Session, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return Session{}, ErrUnauthorized
	}
	var session Session
	if err := verifyJSON(m.key, cookie.Value, &session); err != nil {
		return Session{}, ErrUnauthorized
	}
	if session.Principal.Subject == "" || session.CSRFToken == "" || m.clock().UTC().Unix() > session.ExpiresAt {
		return Session{}, ErrUnauthorized
	}
	return session, nil
}

func (m *SessionManager) Clear(w http.ResponseWriter) {
	http.SetCookie(w, m.cookie(sessionCookieName, "", -1))
}

func (m *SessionManager) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

type OIDCLogin struct {
	clientID      string
	roles         roleAuthorizer
	oauth2Config  oauth2.Config
	verifier      *oidc.IDTokenVerifier
	sessions      *SessionManager
	sessionKey    []byte
	clock         func() time.Time
	secureCookies bool
}

type oauthState struct {
	State        string `json:"state"`
	Nonce        string `json:"nonce"`
	CodeVerifier string `json:"codeVerifier"`
	ReturnTo     string `json:"returnTo"`
	ExpiresAt    int64  `json:"exp"`
}

func NewOIDCLogin(ctx context.Context, cfg Config, sessions *SessionManager) (*OIDCLogin, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	return &OIDCLogin{
		clientID: cfg.OIDCClientID,
		roles:    newRoleAuthorizer(cfg.RequiredRoles),
		oauth2Config: oauth2.Config{
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  strings.TrimRight(cfg.PublicBaseURL, "/") + "/auth/callback",
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier:      provider.Verifier(&oidc.Config{ClientID: cfg.OIDCClientID}),
		sessions:      sessions,
		sessionKey:    cfg.SessionAuthKey,
		clock:         cfg.Clock,
		secureCookies: strings.HasPrefix(cfg.PublicBaseURL, "https://"),
	}, nil
}

func (l *OIDCLogin) Start(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	state, err := randomToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to start login")
		return
	}
	nonce, err := randomToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to start login")
		return
	}
	codeVerifier, err := randomToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to start login")
		return
	}
	payload := oauthState{
		State:        state,
		Nonce:        nonce,
		CodeVerifier: codeVerifier,
		ReturnTo:     cleanReturnTo(r.URL.Query().Get("return_to")),
		ExpiresAt:    l.clock().UTC().Add(oauthStateTTL).Unix(),
	}
	stateToken, err := signJSON(l.sessionKey, payload)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to start login")
		return
	}
	http.SetCookie(w, l.stateCookie(stateToken, int(oauthStateTTL.Seconds())))
	authURL := l.oauth2Config.AuthCodeURL(
		state,
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.SetAuthURLParam("code_challenge", s256Challenge(codeVerifier)),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (l *OIDCLogin) Complete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if oidcError := strings.TrimSpace(r.URL.Query().Get("error")); oidcError != "" {
		writeJSONError(w, http.StatusUnauthorized, "login failed: "+oidcError)
		return
	}
	stateCookie, err := r.Cookie(oauthStateCookieName)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "login state missing")
		return
	}
	http.SetCookie(w, l.stateCookie("", -1))
	var state oauthState
	if err := verifyJSON(l.sessionKey, stateCookie.Value, &state); err != nil {
		writeJSONError(w, http.StatusUnauthorized, "login state invalid")
		return
	}
	if l.clock().UTC().Unix() > state.ExpiresAt || state.State == "" || state.State != r.URL.Query().Get("state") {
		writeJSONError(w, http.StatusUnauthorized, "login state invalid")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		writeJSONError(w, http.StatusUnauthorized, "login code missing")
		return
	}
	token, err := l.oauth2Config.Exchange(r.Context(), code, oauth2.SetAuthURLParam("code_verifier", state.CodeVerifier))
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "login token exchange failed")
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		writeJSONError(w, http.StatusUnauthorized, "login id token missing")
		return
	}
	idToken, err := l.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "login id token invalid")
		return
	}
	var claims struct {
		keycloakClaims
		Nonce string `json:"nonce"`
	}
	if err := idToken.Claims(&claims); err != nil {
		writeJSONError(w, http.StatusUnauthorized, "login claims invalid")
		return
	}
	if claims.Nonce != state.Nonce {
		writeJSONError(w, http.StatusUnauthorized, "login nonce invalid")
		return
	}
	if !l.roles.Allows(claims.keycloakClaims, l.clientID) {
		writeAuthError(w, ErrForbidden)
		return
	}
	_, err = l.sessions.Create(w, principalFromClaims(idToken.Subject, claims.keycloakClaims, l.clientID))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	http.Redirect(w, r, cleanReturnTo(state.ReturnTo), http.StatusFound)
}

func (l *OIDCLogin) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	l.sessions.Clear(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (l *OIDCLogin) stateCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    value,
		Path:     "/auth",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   l.secureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

func requireCSRF(authenticated AuthenticatedRequest, r *http.Request) error {
	if !authenticated.RequiresCSRF {
		return nil
	}
	token := strings.TrimSpace(r.Header.Get(csrfHeaderName))
	if token == "" || token != authenticated.CSRFToken {
		return ErrForbidden
	}
	return nil
}

type keycloakClaims struct {
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
	ResourceAccess map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`
}

type roleAuthorizer struct {
	required map[string]struct{}
}

func newRoleAuthorizer(required []string) roleAuthorizer {
	roles := make(map[string]struct{}, len(required))
	for _, role := range required {
		role = strings.TrimSpace(role)
		if role != "" {
			roles[role] = struct{}{}
		}
	}
	return roleAuthorizer{required: roles}
}

func (a roleAuthorizer) Allows(claims keycloakClaims, clientID string) bool {
	if len(a.required) == 0 {
		return false
	}
	for _, role := range collectRoles(claims, clientID) {
		if _, ok := a.required[role]; ok {
			return true
		}
	}
	return false
}

func principalFromClaims(subject string, claims keycloakClaims, clientID string) Principal {
	return Principal{
		Subject:           subject,
		Email:             claims.Email,
		PreferredUsername: claims.PreferredUsername,
		Roles:             collectRoles(claims, clientID),
	}
}

func collectRoles(claims keycloakClaims, clientID string) []string {
	seen := map[string]bool{}
	var roles []string
	add := func(values []string) {
		for _, role := range values {
			role = strings.TrimSpace(role)
			if role == "" || seen[role] {
				continue
			}
			seen[role] = true
			roles = append(roles, role)
		}
	}
	add(claims.RealmAccess.Roles)
	if claims.ResourceAccess != nil {
		if resource, ok := claims.ResourceAccess[clientID]; ok {
			add(resource.Roles)
		}
	}
	return roles
}

func bearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if value == "" {
		return ""
	}
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func cleanReturnTo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "/"
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") {
		return "/"
	}
	if strings.HasPrefix(parsed.Path, "//") || strings.HasPrefix(parsed.Path, "/auth/") {
		return "/"
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.RequestURI()
}

func randomToken() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func signJSON(key []byte, payload any) (string, error) {
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

func verifyJSON(key []byte, token string, target any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return errors.New("invalid signed value")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("invalid signed value signature")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return errors.New("invalid signed value signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return errors.New("invalid signed value payload")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("invalid signed value payload: %w", err)
	}
	return nil
}

func writeAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrForbidden) {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	writeJSONError(w, http.StatusUnauthorized, "unauthorized")
}
