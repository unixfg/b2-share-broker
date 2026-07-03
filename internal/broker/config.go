package broker

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddr      = ":8080"
	defaultRegion          = "us-west-004"
	defaultMaxUploadBytes  = int64(512 * 1024 * 1024)
	defaultObjectPrefix    = "s"
	defaultPresignTTL      = 15 * time.Minute
	defaultUploadTokenTTL  = time.Hour
	defaultSessionTTL      = 12 * time.Hour
	minUploadTokenKeyBytes = 32
)

type Config struct {
	ListenAddr        string
	IssuerURL         string
	OIDCClientID      string
	OIDCClientSecret  string
	AllowedSubjects   []string
	B2Endpoint        string
	B2Region          string
	B2Bucket          string
	B2PublicBaseURL   string
	PublicBaseURL     string
	B2AccessKeyID     string
	B2SecretAccessKey string
	DatabaseURL       string
	ObjectPrefix      string
	MaxUploadBytes    int64
	PresignTTL        time.Duration
	UploadTokenTTL    time.Duration
	UploadTokenKey    []byte
	AliasHMACKey      []byte
	SessionTTL        time.Duration
	SessionAuthKey    []byte
	Clock             func() time.Time
}

func LoadConfigFromEnv() (Config, error) {
	listenAddr := envString("LISTEN_ADDR", "")
	if listenAddr == "" {
		if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
			listenAddr = ":" + port
		} else {
			listenAddr = defaultListenAddr
		}
	}

	tokenKey, err := parseSecretKey(os.Getenv("UPLOAD_TOKEN_KEY"))
	if err != nil {
		return Config{}, err
	}
	sessionAuthKey, err := parseSecretKey(os.Getenv("SESSION_AUTH_KEY"))
	if err != nil {
		return Config{}, err
	}
	aliasHMACKey, err := parseSecretKey(os.Getenv("ALIAS_HMAC_KEY"))
	if err != nil {
		return Config{}, err
	}

	b2PublicBaseURL := strings.TrimRight(envString("B2_PUBLIC_BASE_URL", ""), "/")
	cfg := Config{
		ListenAddr:        listenAddr,
		IssuerURL:         envString("OIDC_ISSUER_URL", ""),
		OIDCClientID:      firstEnv("OIDC_CLIENT_ID", "OIDC_AUDIENCE"),
		OIDCClientSecret:  envString("OIDC_CLIENT_SECRET", ""),
		AllowedSubjects:   envList("OIDC_ALLOWED_SUBJECTS"),
		B2Endpoint:        strings.TrimRight(envString("B2_ENDPOINT", ""), "/"),
		B2Region:          envString("B2_REGION", defaultRegion),
		B2Bucket:          envString("B2_BUCKET", ""),
		B2PublicBaseURL:   b2PublicBaseURL,
		PublicBaseURL:     strings.TrimRight(envString("PUBLIC_BASE_URL", b2PublicBaseURL), "/"),
		B2AccessKeyID:     firstEnv("AWS_ACCESS_KEY_ID", "ACCESS_KEY_ID"),
		B2SecretAccessKey: firstEnv("AWS_SECRET_ACCESS_KEY", "ACCESS_SECRET_KEY"),
		DatabaseURL:       envString("DATABASE_URL", ""),
		ObjectPrefix:      strings.Trim(envString("OBJECT_PREFIX", defaultObjectPrefix), "/"),
		MaxUploadBytes:    envInt64("MAX_UPLOAD_BYTES", defaultMaxUploadBytes),
		PresignTTL:        envDurationSeconds("PRESIGN_TTL_SECONDS", defaultPresignTTL),
		UploadTokenTTL:    envDurationSeconds("UPLOAD_TOKEN_TTL_SECONDS", defaultUploadTokenTTL),
		UploadTokenKey:    tokenKey,
		AliasHMACKey:      aliasHMACKey,
		SessionTTL:        envDurationSeconds("SESSION_TTL_SECONDS", defaultSessionTTL),
		SessionAuthKey:    sessionAuthKey,
		Clock:             time.Now,
	}

	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	var missing []string
	require := func(name, value string) {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}

	require("OIDC_ISSUER_URL", c.IssuerURL)
	require("OIDC_CLIENT_ID", c.OIDCClientID)
	require("OIDC_CLIENT_SECRET", c.OIDCClientSecret)
	require("B2_ENDPOINT", c.B2Endpoint)
	require("B2_BUCKET", c.B2Bucket)
	require("B2_PUBLIC_BASE_URL", c.B2PublicBaseURL)
	require("PUBLIC_BASE_URL", c.PublicBaseURL)
	require("AWS_ACCESS_KEY_ID or ACCESS_KEY_ID", c.B2AccessKeyID)
	require("AWS_SECRET_ACCESS_KEY or ACCESS_SECRET_KEY", c.B2SecretAccessKey)
	require("DATABASE_URL", c.DatabaseURL)
	if len(c.UploadTokenKey) < minUploadTokenKeyBytes {
		missing = append(missing, "UPLOAD_TOKEN_KEY")
	}
	if len(c.AliasHMACKey) < minUploadTokenKeyBytes {
		missing = append(missing, "ALIAS_HMAC_KEY")
	}
	if len(c.SessionAuthKey) < minUploadTokenKeyBytes {
		missing = append(missing, "SESSION_AUTH_KEY")
	}
	if len(c.AllowedSubjects) == 0 {
		missing = append(missing, "OIDC_ALLOWED_SUBJECTS")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}

	if _, err := url.ParseRequestURI(c.IssuerURL); err != nil {
		return fmt.Errorf("OIDC_ISSUER_URL is invalid: %w", err)
	}
	if _, err := url.ParseRequestURI(c.B2Endpoint); err != nil {
		return fmt.Errorf("B2_ENDPOINT is invalid: %w", err)
	}
	if _, err := url.ParseRequestURI(c.B2PublicBaseURL); err != nil {
		return fmt.Errorf("B2_PUBLIC_BASE_URL is invalid: %w", err)
	}
	if _, err := url.ParseRequestURI(c.PublicBaseURL); err != nil {
		return fmt.Errorf("PUBLIC_BASE_URL is invalid: %w", err)
	}
	if parsed, err := url.Parse(c.DatabaseURL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
		if err != nil {
			return fmt.Errorf("DATABASE_URL is invalid: %w", err)
		}
		return errors.New("DATABASE_URL is invalid")
	}
	if c.MaxUploadBytes <= 0 {
		return errors.New("MAX_UPLOAD_BYTES must be positive")
	}
	if c.PresignTTL <= 0 {
		return errors.New("PRESIGN_TTL_SECONDS must be positive")
	}
	if c.UploadTokenTTL <= 0 {
		return errors.New("UPLOAD_TOKEN_TTL_SECONDS must be positive")
	}
	if c.SessionTTL <= 0 {
		return errors.New("SESSION_TTL_SECONDS must be positive")
	}
	if c.ObjectPrefix == "" {
		return errors.New("OBJECT_PREFIX must not be empty")
	}
	return nil
}

func envString(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envList(name string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		values = append(values, value)
	}
	return values
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func envInt64(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

func envDurationSeconds(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return time.Duration(value) * time.Second
}

func parseSecretKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err == nil && len(decoded) >= minUploadTokenKeyBytes {
		return decoded, nil
	}
	decoded, err = base64.RawURLEncoding.DecodeString(raw)
	if err == nil && len(decoded) >= minUploadTokenKeyBytes {
		return decoded, nil
	}
	if len([]byte(raw)) < minUploadTokenKeyBytes {
		return nil, fmt.Errorf("UPLOAD_TOKEN_KEY must be at least %d bytes or a base64 value that decodes to at least %d bytes", minUploadTokenKeyBytes, minUploadTokenKeyBytes)
	}
	return []byte(raw), nil
}
