package broker

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// GitHubApp mints installation access tokens.
//
// This provider exists because GitHub is the one common case where every part
// of the ephemeral-credential story actually works. An installation token
// lasts exactly one hour and cannot be extended; it can be narrowed to
// specific repositories and specific permissions; and, unusually, it can be
// revoked outright. Most providers offer at best a short lifetime and no way
// to end it early -- AWS STS has no per-session revocation at all, only a
// policy that invalidates every session issued before now.
//
// So a bay that needs GitHub access gets a token scoped to the repositories it
// works on, and destroying the bay destroys the token rather than waiting an
// hour for it to lapse.
//
// A reference is "github/<installation-id>" or just "github" when
// DEVBAY_GITHUB_INSTALLATION_ID is set.
type GitHubApp struct {
	// AppID is the application's numeric id.
	AppID string
	// PrivateKeyPEM is the app's signing key. Read from disk by the caller so
	// this type never touches the filesystem.
	PrivateKeyPEM []byte
	// InstallationID is used when a reference does not name one.
	InstallationID string
	// Repositories narrows the token. Empty means every repository the
	// installation can see, which is worth avoiding.
	Repositories []string
	// Permissions narrows it further, e.g. {"contents": "read"}.
	Permissions map[string]string

	// BaseURL is the API root; overridden in tests and for GitHub Enterprise.
	BaseURL string
	// HTTP is the client used; overridden in tests.
	HTTP *http.Client
	// Now is the clock, overridden in tests.
	Now func() time.Time
}

// GitHubAppFromEnv builds a provider from the environment, or returns nil when
// it is not configured.
//
// Configuration comes from the environment rather than from a manifest by
// design: a private key is exactly the kind of thing that must never be
// expressible in a file the introspection agent can write.
func GitHubAppFromEnv() (*GitHubApp, error) {
	appID := os.Getenv("DEVBAY_GITHUB_APP_ID")
	keyPath := os.Getenv("DEVBAY_GITHUB_APP_KEY")
	if appID == "" || keyPath == "" {
		return nil, nil
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("reading DEVBAY_GITHUB_APP_KEY: %w", err)
	}
	g := &GitHubApp{
		AppID:          appID,
		PrivateKeyPEM:  key,
		InstallationID: os.Getenv("DEVBAY_GITHUB_INSTALLATION_ID"),
		// Configurable because GitHub Enterprise exists, and because a
		// self-hosted instance is exactly the case where a per-bay token
		// matters most.
		BaseURL: os.Getenv("DEVBAY_GITHUB_API_URL"),
	}
	if repos := os.Getenv("DEVBAY_GITHUB_REPOS"); repos != "" {
		g.Repositories = strings.Split(repos, ",")
	}
	return g, nil
}

func (g *GitHubApp) Name() string { return "github-app" }

func (g *GitHubApp) Handles(ref string) bool {
	return ref == "github" || strings.HasPrefix(ref, "github/")
}

func (g *GitHubApp) baseURL() string {
	if g.BaseURL != "" {
		return strings.TrimSuffix(g.BaseURL, "/")
	}
	return "https://api.github.com"
}

func (g *GitHubApp) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *GitHubApp) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// Resolve mints a token for a bay.
func (g *GitHubApp) Resolve(ctx context.Context, bay, ref string) (string, *Grant, error) {
	installation := g.InstallationID
	if _, id, ok := strings.Cut(ref, "/"); ok && id != "" {
		installation = id
	}
	if installation == "" {
		return "", nil, errors.New("no installation id; set DEVBAY_GITHUB_INSTALLATION_ID or use github/<id>")
	}

	jwt, err := g.appJWT()
	if err != nil {
		return "", nil, err
	}

	body := map[string]any{}
	if len(g.Repositories) > 0 {
		body["repositories"] = g.Repositories
	}
	if len(g.Permissions) > 0 {
		body["permissions"] = g.Permissions
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", nil, err
	}

	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", g.baseURL(), installation)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client().Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
		Message   string    `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", nil, fmt.Errorf("decoding the token response: %w", err)
	}
	if resp.StatusCode >= 300 || out.Token == "" {
		msg := out.Message
		if msg == "" {
			msg = resp.Status
		}
		return "", nil, fmt.Errorf("minting a token: %s", msg)
	}

	token := out.Token
	grant := &Grant{
		Provider:  g.Name(),
		Bay:       bay,
		IssuedAt:  g.now(),
		ExpiresAt: out.ExpiresAt,
		Minted:    true,
		revoke: func(ctx context.Context) error {
			// The token authenticates its own destruction, which is why this
			// closure holds it: there is no separate credential to keep.
			return g.revoke(ctx, token)
		},
	}
	return token, grant, nil
}

// revoke destroys an installation token immediately.
func (g *GitHubApp) revoke(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, g.baseURL()+"/installation/token", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		// Already gone, most likely expired. Teardown runs after crashes and
		// long gaps, so this is a success rather than a failure.
		return nil
	default:
		return fmt.Errorf("revoking: %s", resp.Status)
	}
}

// appJWT builds the short-lived assertion that authenticates as the app.
//
// GitHub requires RS256 and rejects anything issued in the future, so the
// issued-at is backdated by a minute to absorb clock skew between this machine
// and theirs -- a skew of a few seconds is common and produces a bewildering
// 401 otherwise.
func (g *GitHubApp) appJWT() (string, error) {
	key, err := parseRSAKey(g.PrivateKeyPEM)
	if err != nil {
		return "", err
	}

	now := g.now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		// GitHub caps the lifetime at ten minutes and rejects more.
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": g.AppID,
	}

	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(h) + "." + enc.EncodeToString(c)

	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing the app assertion: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// parseRSAKey accepts both PEM encodings GitHub has handed out over the years.
func parseRSAKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("the private key is not PEM-encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing the private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the private key is not RSA; GitHub apps require RSA")
	}
	return key, nil
}
