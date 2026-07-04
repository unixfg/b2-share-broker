package broker

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionManagerCreatesAndReadsSession(t *testing.T) {
	cfg := testConfig(t)
	manager := NewSessionManager(cfg)
	recorder := httptest.NewRecorder()

	session, err := manager.Create(recorder, Principal{Subject: "user-1", Email: "user@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if session.CSRFToken == "" {
		t.Fatal("CSRF token is empty")
	}
	response := recorder.Result()
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %#v", cookies)
	}
	if !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %#v", cookies[0])
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookies[0])
	readSession, err := manager.Read(request)
	if err != nil {
		t.Fatal(err)
	}
	if readSession.Principal.Subject != "user-1" || readSession.Principal.Email != "user@example.test" || readSession.CSRFToken != session.CSRFToken {
		t.Fatalf("session = %#v", readSession)
	}
}

func TestSessionManagerRejectsExpiredSession(t *testing.T) {
	cfg := testConfig(t)
	now := cfg.Clock()
	cfg.Clock = func() time.Time { return now }
	manager := NewSessionManager(cfg)
	recorder := httptest.NewRecorder()
	if _, err := manager.Create(recorder, Principal{Subject: "user-1"}); err != nil {
		t.Fatal(err)
	}
	cookie := recorder.Result().Cookies()[0]

	cfg.Clock = func() time.Time { return now.Add(cfg.SessionTTL + time.Second) }
	expiredManager := NewSessionManager(cfg)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookie)
	if _, err := expiredManager.Read(request); err == nil {
		t.Fatal("expected expired session error")
	}
}

func TestRequireCSRF(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", nil)
	authenticated := AuthenticatedRequest{CSRFToken: "csrf-token", RequiresCSRF: true}
	if err := requireCSRF(authenticated, request); err == nil {
		t.Fatal("expected missing CSRF error")
	}
	request.Header.Set(csrfHeaderName, "csrf-token")
	if err := requireCSRF(authenticated, request); err != nil {
		t.Fatal(err)
	}
}

func TestRequireCSRFSkipsBearerRequests(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/uploads", nil)
	authenticated := AuthenticatedRequest{CSRFToken: "csrf-token", RequiresCSRF: false}
	if err := requireCSRF(authenticated, request); err != nil {
		t.Fatal(err)
	}
}

func TestRoleAuthorizerAllowsRealmOrClientRole(t *testing.T) {
	authorizer := newRoleAuthorizer([]string{"b2-share-user"})
	var realmClaims keycloakClaims
	realmClaims.RealmAccess.Roles = []string{"b2-share-user"}
	if !authorizer.Allows(realmClaims, "b2-share-web") {
		t.Fatal("expected realm role to authorize")
	}

	var clientClaims keycloakClaims
	clientClaims.ResourceAccess = map[string]struct {
		Roles []string `json:"roles"`
	}{
		"b2-share-web": {Roles: []string{"b2-share-user"}},
	}
	if !authorizer.Allows(clientClaims, "b2-share-web") {
		t.Fatal("expected client role to authorize")
	}
	if authorizer.Allows(clientClaims, "other-client") {
		t.Fatal("expected role on another client to be ignored")
	}
}

func TestCleanReturnToRejectsExternalAndAuthTargets(t *testing.T) {
	tests := map[string]string{
		"":                           "/",
		"/share?shared=1":            "/share?shared=1",
		"https://example.test/share": "/",
		"//example.test/share":       "/",
		"/auth/callback":             "/",
	}
	for input, want := range tests {
		if got := cleanReturnTo(input); got != want {
			t.Fatalf("cleanReturnTo(%q) = %q, want %q", input, got, want)
		}
	}
}
