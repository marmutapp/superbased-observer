package m365copilotanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// clientSecretEnv is the environment variable the admin uses to supply the Entra
// app's client secret without writing it into the server TOML. Mirrors the
// {CC,CODEX,COPILOT}_ANALYTICS key-env convention.
const clientSecretEnv = "M365_COPILOT_CLIENT_SECRET"

// graphScope is the app-only Microsoft Graph scope for the client-credentials
// flow. .default requests every application permission admin-consented to the
// app (here AiEnterpriseInteraction.Read.All + optionally User.Read.All) — the
// client-credentials grant cannot request dynamic scopes.
const graphScope = "https://graph.microsoft.com/.default"

// defaultLoginBaseURL is the Entra (Azure AD) v2.0 token authority. Tenant-scoped:
// the token endpoint is {authority}/{tenant}/oauth2/v2.0/token.
const defaultLoginBaseURL = "https://login.microsoftonline.com"

// ResolveClientSecret returns the Entra app client secret from the env var
// (preferred) or the secret file at secretFile. It never logs or returns the
// secret in an error.
func ResolveClientSecret(secretFile string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(clientSecretEnv)); v != "" {
		return v, nil
	}
	if secretFile != "" {
		b, err := os.ReadFile(secretFile)
		if err != nil {
			return "", fmt.Errorf("m365copilotanalytics: read client_secret_file: %w", err)
		}
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("m365copilotanalytics: no client secret (set %s or client_secret_file)", clientSecretEnv)
}

// TokenSource performs the Entra app-only OAuth2 client-credentials flow and
// caches the bearer token until shortly before it expires. It is the single place
// the client secret reaches the wire. HTTPClient + LoginBaseURL are injectable so
// tests drive it against an httptest server; delegated flows are NOT supported
// (app-only by design).
type TokenSource struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	LoginBaseURL string // default https://login.microsoftonline.com; test override
	HTTPClient   *http.Client
	Now          func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewTokenSource builds a TokenSource. An empty LoginBaseURL defaults to the
// public Entra authority. It does not validate credentials until the first Token
// call (which is where a bad tenant/secret surfaces).
func NewTokenSource(tenantID, clientID, clientSecret, loginBaseURL string, client *http.Client) *TokenSource {
	base := strings.TrimRight(strings.TrimSpace(loginBaseURL), "/")
	if base == "" {
		base = defaultLoginBaseURL
	}
	return &TokenSource{
		TenantID:     strings.TrimSpace(tenantID),
		ClientID:     strings.TrimSpace(clientID),
		ClientSecret: clientSecret,
		LoginBaseURL: base,
		HTTPClient:   client,
	}
}

// tokenResponse is the Entra token endpoint's success shape.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"` // seconds
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// tokenExpiryMargin is subtracted from expires_in so a token is refreshed before
// Graph would reject it mid-poll.
const tokenExpiryMargin = 60 * time.Second

// Token returns a valid bearer token, fetching (and caching) a new one via the
// client-credentials grant when the cached token is absent or near expiry.
func (ts *TokenSource) Token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	now := time.Now
	if ts.Now != nil {
		now = ts.Now
	}
	if ts.token != "" && now().Before(ts.expires) {
		return ts.token, nil
	}
	if ts.TenantID == "" || ts.ClientID == "" || ts.ClientSecret == "" {
		return "", fmt.Errorf("m365copilotanalytics: incomplete Entra app credentials (tenant/client id/secret required)")
	}

	form := url.Values{}
	form.Set("client_id", ts.ClientID)
	form.Set("client_secret", ts.ClientSecret)
	form.Set("scope", graphScope)
	form.Set("grant_type", "client_credentials")

	endpoint := ts.LoginBaseURL + "/" + url.PathEscape(ts.TenantID) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("m365copilotanalytics: new token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	client := ts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("m365copilotanalytics: do token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("m365copilotanalytics: read token body: %w", err)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("m365copilotanalytics: parse token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		// tr.Error is Entra's machine code (e.g. invalid_client); never echo the
		// secret. ErrorDesc can be verbose but is not secret-bearing.
		return "", fmt.Errorf("m365copilotanalytics: token endpoint %d: %s", resp.StatusCode, firstNonEmpty(tr.Error, "no access_token"))
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= tokenExpiryMargin {
		ttl = tokenExpiryMargin + time.Minute
	}
	ts.token = tr.AccessToken
	ts.expires = now().Add(ttl - tokenExpiryMargin)
	return ts.token, nil
}
