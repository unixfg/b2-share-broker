package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)

type Principal struct {
	Subject           string
	Email             string
	PreferredUsername string
}

type Authenticator interface {
	Authenticate(ctx context.Context, authorizationHeader string) (Principal, error)
}

type OIDCAuthenticator struct {
	verifier *oidc.IDTokenVerifier
	allowed  map[string]struct{}
}

func NewOIDCAuthenticator(ctx context.Context, issuerURL, audience string, allowedSubjects []string) (*OIDCAuthenticator, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(allowedSubjects))
	for _, subject := range allowedSubjects {
		subject = strings.TrimSpace(subject)
		if subject != "" {
			allowed[subject] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return nil, errors.New("at least one allowed OIDC subject is required")
	}
	return &OIDCAuthenticator{
		verifier: provider.Verifier(&oidc.Config{ClientID: audience}),
		allowed:  allowed,
	}, nil
}

func (a *OIDCAuthenticator) Authenticate(ctx context.Context, authorizationHeader string) (Principal, error) {
	rawToken, ok := bearerToken(authorizationHeader)
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	idToken, err := a.verifier.Verify(ctx, rawToken)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if _, ok := a.allowed[idToken.Subject]; !ok {
		return Principal{}, ErrForbidden
	}

	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
	}
	_ = idToken.Claims(&claims)
	return Principal{
		Subject:           idToken.Subject,
		Email:             claims.Email,
		PreferredUsername: claims.PreferredUsername,
	}, nil
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	header = strings.TrimSpace(header)
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func writeAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrForbidden) {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	writeJSONError(w, http.StatusUnauthorized, "unauthorized")
}
