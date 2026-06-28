package sharecli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"
)

const (
	defaultBrokerURL = "https://share.doesthings.io"
	defaultIssuerURL = "https://auth.doesthings.io/realms/doesthings.io"
	defaultClientID  = "b2-share-broker"
	keyringService   = "b2-share"
)

type tokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

type issuerConfig struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

type createUploadRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

type createUploadResponse struct {
	UploadURL       string            `json:"uploadUrl"`
	RequiredHeaders map[string]string `json:"requiredHeaders"`
	UploadToken     string            `json:"uploadToken"`
	PublicURL       string            `json:"publicUrl"`
}

type completeUploadResponse struct {
	PublicURL string `json:"publicUrl"`
	Verified  bool   `json:"verified"`
}

type commandConfig struct {
	BrokerURL  string
	IssuerURL  string
	ClientID   string
	HTTPClient *http.Client
	Stdout     io.Writer
	Stderr     io.Writer
	OpenURL    func(string) error
}

func NewCommand() *cobra.Command {
	cfg := commandConfig{
		BrokerURL:  envString("B2_SHARE_BROKER_URL", defaultBrokerURL),
		IssuerURL:  envString("B2_SHARE_OIDC_ISSUER", defaultIssuerURL),
		ClientID:   envString("B2_SHARE_OIDC_CLIENT_ID", defaultClientID),
		HTTPClient: &http.Client{Timeout: 10 * time.Minute},
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		OpenURL:    openBrowser,
	}

	root := &cobra.Command{
		Use:   "b2-share",
		Short: "Upload files through b2-share-broker",
	}
	root.PersistentFlags().StringVar(&cfg.BrokerURL, "broker-url", cfg.BrokerURL, "broker API base URL")
	root.PersistentFlags().StringVar(&cfg.IssuerURL, "issuer-url", cfg.IssuerURL, "OIDC issuer URL")
	root.PersistentFlags().StringVar(&cfg.ClientID, "client-id", cfg.ClientID, "OIDC public client ID")

	uploadCmd := &cobra.Command{
		Use:   "upload <path>",
		Short: "Upload one file and copy the public URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpload(cmd.Context(), cfg, args[0])
		},
	}

	loginCmd := &cobra.Command{
		Use:   "login",
		Short: "Refresh the stored OIDC token",
		RunE: func(cmd *cobra.Command, args []string) error {
			issuer, err := discoverIssuer(cmd.Context(), cfg.HTTPClient, cfg.IssuerURL)
			if err != nil {
				return err
			}
			token, err := login(cmd.Context(), cfg, issuer)
			if err != nil {
				return err
			}
			if err := saveToken(cfg.ClientID, token); err != nil {
				return err
			}
			fmt.Fprintln(cfg.Stdout, "login complete")
			return nil
		},
	}

	root.AddCommand(uploadCmd, loginCmd)
	return root
}

func runUpload(ctx context.Context, cfg commandConfig, path string) error {
	stat, err := os.Stat(path)
	if err != nil {
		return err
	}
	if stat.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	contentType, err := detectContentType(path, file)
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}

	accessToken, err := accessToken(ctx, cfg)
	if err != nil {
		return err
	}

	upload, err := createUpload(ctx, cfg, accessToken, createUploadRequest{
		Filename:    filepath.Base(path),
		ContentType: contentType,
		Size:        stat.Size(),
	})
	if err != nil {
		return err
	}
	if err := putFile(ctx, cfg.HTTPClient, upload, file, stat.Size(), contentType); err != nil {
		return err
	}
	completed, err := completeUpload(ctx, cfg, accessToken, upload.UploadToken)
	if err != nil {
		return err
	}
	publicURL := completed.PublicURL
	if publicURL == "" {
		publicURL = upload.PublicURL
	}

	fmt.Fprintln(cfg.Stdout, publicURL)
	if err := clipboard.WriteAll(publicURL); err != nil {
		fmt.Fprintf(cfg.Stderr, "clipboard unavailable: %v\n", err)
	}
	return nil
}

func accessToken(ctx context.Context, cfg commandConfig) (string, error) {
	issuer, err := discoverIssuer(ctx, cfg.HTTPClient, cfg.IssuerURL)
	if err != nil {
		return "", err
	}
	if token, ok := loadToken(cfg.ClientID); ok {
		if token.AccessToken != "" && time.Until(token.Expiry) > 60*time.Second {
			return token.AccessToken, nil
		}
		if token.RefreshToken != "" {
			refreshed, err := refreshToken(ctx, cfg, issuer, token.RefreshToken)
			if err == nil {
				_ = saveToken(cfg.ClientID, refreshed)
				return refreshed.AccessToken, nil
			}
			fmt.Fprintf(cfg.Stderr, "token refresh failed, starting login: %v\n", err)
		}
	}
	token, err := login(ctx, cfg, issuer)
	if err != nil {
		return "", err
	}
	if err := saveToken(cfg.ClientID, token); err != nil {
		fmt.Fprintf(cfg.Stderr, "warning: token was not stored: %v\n", err)
	}
	return token.AccessToken, nil
}

func discoverIssuer(ctx context.Context, client *http.Client, issuerURL string) (issuerConfig, error) {
	wellKnown := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return issuerConfig{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return issuerConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return issuerConfig{}, fmt.Errorf("issuer discovery returned %s", resp.Status)
	}
	var config issuerConfig
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return issuerConfig{}, err
	}
	if config.AuthorizationEndpoint == "" || config.TokenEndpoint == "" {
		return issuerConfig{}, errors.New("issuer discovery response is missing authorization or token endpoint")
	}
	return config, nil
}

func login(ctx context.Context, cfg commandConfig, issuer issuerConfig) (tokenSet, error) {
	state, err := randomURLValue(32)
	if err != nil {
		return tokenSet{}, err
	}
	verifier, err := randomURLValue(48)
	if err != nil {
		return tokenSet{}, err
	}
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return tokenSet{}, err
	}
	defer listener.Close()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	redirectURI := "http://" + listener.Addr().String() + "/callback"
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			if r.URL.Query().Get("state") != state {
				http.Error(w, "invalid state", http.StatusBadRequest)
				errCh <- errors.New("OIDC callback state mismatch")
				return
			}
			if oidcError := r.URL.Query().Get("error"); oidcError != "" {
				http.Error(w, oidcError, http.StatusBadRequest)
				errCh <- fmt.Errorf("OIDC login failed: %s", oidcError)
				return
			}
			code := r.URL.Query().Get("code")
			if code == "" {
				http.Error(w, "missing code", http.StatusBadRequest)
				errCh <- errors.New("OIDC callback missing code")
				return
			}
			fmt.Fprintln(w, "b2-share login complete. You can close this window.")
			codeCh <- code
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	defer server.Shutdown(context.Background())

	authURL, err := url.Parse(issuer.AuthorizationEndpoint)
	if err != nil {
		return tokenSet{}, err
	}
	values := authURL.Query()
	values.Set("response_type", "code")
	values.Set("client_id", cfg.ClientID)
	values.Set("redirect_uri", redirectURI)
	values.Set("scope", "openid profile email offline_access")
	values.Set("state", state)
	values.Set("code_challenge", challenge)
	values.Set("code_challenge_method", "S256")
	authURL.RawQuery = values.Encode()

	fmt.Fprintf(cfg.Stderr, "Open this URL to sign in:\n%s\n", authURL.String())
	if err := cfg.OpenURL(authURL.String()); err != nil {
		fmt.Fprintf(cfg.Stderr, "browser open failed: %v\n", err)
	}

	select {
	case code := <-codeCh:
		return exchangeCode(ctx, cfg, issuer, code, redirectURI, verifier)
	case err := <-errCh:
		return tokenSet{}, err
	case <-time.After(5 * time.Minute):
		return tokenSet{}, errors.New("timed out waiting for OIDC login")
	case <-ctx.Done():
		return tokenSet{}, ctx.Err()
	}
}

func exchangeCode(ctx context.Context, cfg commandConfig, issuer issuerConfig, code, redirectURI, verifier string) (tokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", cfg.ClientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)
	return tokenRequest(ctx, cfg.HTTPClient, issuer.TokenEndpoint, form)
}

func refreshToken(ctx context.Context, cfg commandConfig, issuer issuerConfig, refreshToken string) (tokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", cfg.ClientID)
	form.Set("refresh_token", refreshToken)
	return tokenRequest(ctx, cfg.HTTPClient, issuer.TokenEndpoint, form)
}

func tokenRequest(ctx context.Context, client *http.Client, endpoint string, form url.Values) (tokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenSet{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return tokenSet{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return tokenSet{}, fmt.Errorf("token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return tokenSet{}, err
	}
	if payload.AccessToken == "" {
		return tokenSet{}, errors.New("token endpoint did not return an access token")
	}
	if payload.ExpiresIn <= 0 {
		payload.ExpiresIn = 300
	}
	return tokenSet{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}, nil
}

func createUpload(ctx context.Context, cfg commandConfig, accessToken string, payload createUploadRequest) (createUploadResponse, error) {
	body, _ := json.Marshal(payload)
	endpoint := strings.TrimRight(cfg.BrokerURL, "/") + "/api/uploads"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return createUploadResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return createUploadResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return createUploadResponse{}, fmt.Errorf("broker create upload returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var response createUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return createUploadResponse{}, err
	}
	if response.UploadURL == "" || response.UploadToken == "" || response.PublicURL == "" {
		return createUploadResponse{}, errors.New("broker create upload response is missing required fields")
	}
	return response, nil
}

func putFile(ctx context.Context, client *http.Client, upload createUploadResponse, file *os.File, size int64, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, upload.UploadURL, file)
	if err != nil {
		return err
	}
	req.ContentLength = size
	for name, value := range upload.RequiredHeaders {
		req.Header.Set(name, value)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("direct B2 upload returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func completeUpload(ctx context.Context, cfg commandConfig, accessToken, uploadToken string) (completeUploadResponse, error) {
	body, _ := json.Marshal(map[string]string{"uploadToken": uploadToken})
	endpoint := strings.TrimRight(cfg.BrokerURL, "/") + "/api/uploads/complete"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return completeUploadResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return completeUploadResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return completeUploadResponse{}, fmt.Errorf("broker complete upload returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var response completeUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return completeUploadResponse{}, err
	}
	if response.PublicURL == "" {
		return completeUploadResponse{}, errors.New("broker complete upload response is missing publicUrl")
	}
	return response, nil
}

func detectContentType(path string, file *os.File) (string, error) {
	if contentType := mime.TypeByExtension(filepath.Ext(path)); contentType != "" {
		return contentType, nil
	}
	var sample [512]byte
	n, err := file.Read(sample[:])
	if err != nil && err != io.EOF {
		return "", err
	}
	if n == 0 {
		return "application/octet-stream", nil
	}
	return http.DetectContentType(sample[:n]), nil
}

func loadToken(account string) (tokenSet, bool) {
	if raw, err := keyring.Get(keyringService, account); err == nil {
		var token tokenSet
		if json.Unmarshal([]byte(raw), &token) == nil && token.AccessToken != "" {
			return token, true
		}
	}
	path, err := tokenFilePath(account)
	if err != nil {
		return tokenSet{}, false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return tokenSet{}, false
	}
	var token tokenSet
	if json.Unmarshal(body, &token) != nil || token.AccessToken == "" {
		return tokenSet{}, false
	}
	return token, true
}

func saveToken(account string, token tokenSet) error {
	body, err := json.Marshal(token)
	if err != nil {
		return err
	}
	if err := keyring.Set(keyringService, account, string(body)); err == nil {
		return nil
	}
	path, err := tokenFilePath(account)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func tokenFilePath(account string) (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	name := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(account)
	return filepath.Join(configDir, "b2-share", name+".json"), nil
}

func randomURLValue(byteCount int) (string, error) {
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func openBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

func envString(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}
