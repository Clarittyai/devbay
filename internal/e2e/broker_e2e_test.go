package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Clarittyai/devbay/internal/bay"
)

// A credential minted for a bay must die with it, through the real teardown
// path rather than by calling the broker directly.
//
// The interesting failure is not "revocation does not work" -- that has its own
// tests -- but "teardown never asked for it", which is invisible from inside
// the package that would have done the revoking.
func TestMintedCredentialsDieWithTheBay(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}

	var (
		mu      sync.Mutex
		minted  int
		revoked int
	)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			minted++
			json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_e2eMintedToken1234567890",
				"expires_at": time.Now().Add(time.Hour).UTC(),
			})
		case r.Method == http.MethodDelete:
			revoked++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer gh.Close()

	// The app is configured from the environment, never from a manifest: a
	// private key must not be expressible in a file a generator can write.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey),
	})
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVBAY_GITHUB_APP_ID", "1")
	t.Setenv("DEVBAY_GITHUB_APP_KEY", keyPath)
	t.Setenv("DEVBAY_GITHUB_INSTALLATION_ID", "42")
	t.Setenv("DEVBAY_GITHUB_API_URL", gh.URL)

	repo := newRepo(t)
	// The fixture's app asks for a GitHub token as well as its own secret.
	manifestWithGitHub := strings.Replace(manifestYAML,
		"      API_TOKEN: ${secret:e2e/token}",
		"      API_TOKEN: ${secret:e2e/token}\n      GITHUB_TOKEN: ${secret:github}", 1)
	writeManifest(t, repo, manifestWithGitHub)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	m, err := bay.Open(ctx, bay.Options{
		Dir:          repo,
		StatePath:    filepath.Join(t.TempDir(), "state.db"),
		WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"),
		AuditPath:    filepath.Join(t.TempDir(), "audit.jsonl"),
		NoProxy:      true,
		Log:          func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err != nil {
		t.Skipf("cannot open manager: %v", err)
	}
	defer m.Close()
	m.SetSecret("e2e/token", canary)

	if _, err := m.Create(ctx, bay.CreateOptions{Name: "minted", Alias: "minted", Boot: true}); err != nil {
		t.Fatalf("creating the bay: %v", err)
	}

	mu.Lock()
	got := minted
	mu.Unlock()
	if got == 0 {
		t.Fatal("no token was minted, so this test proves nothing")
	}

	if err := m.Destroy(ctx, "minted", true); err != nil {
		t.Fatalf("destroying: %v", err)
	}

	mu.Lock()
	gotRevoked := revoked
	mu.Unlock()
	if gotRevoked == 0 {
		t.Error("destroying the bay did not revoke the token it was given")
	}
}
