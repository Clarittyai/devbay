package broker

import (
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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Clarittyai/devbay/internal/scrub"
)

// fake is a source with controllable behaviour.
type fake struct {
	name      string
	prefix    string
	value     string
	err       error
	minted    bool
	expires   time.Time
	revoked   *int
	revokeErr error
	mu        sync.Mutex
}

func (f *fake) Name() string { return f.name }
func (f *fake) Handles(ref string) bool {
	return f.prefix == "" || strings.HasPrefix(ref, f.prefix)
}
func (f *fake) Resolve(_ context.Context, bay, ref string) (string, *Grant, error) {
	if f.err != nil {
		return "", nil, f.err
	}
	if f.value == "" {
		return "", nil, nil
	}
	if !f.minted {
		return f.value, nil, nil
	}
	return f.value, &Grant{
		Minted: true, ExpiresAt: f.expires,
		revoke: func(context.Context) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.revokeErr != nil {
				return f.revokeErr
			}
			if f.revoked != nil {
				*f.revoked++
			}
			return nil
		},
	}, nil
}

func newBroker(t *testing.T) (*Broker, *Audit, *scrub.Scrubber) {
	t.Helper()
	a, err := OpenAudit(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sc := scrub.New()
	return New(a, sc, func(string, ...any) {}), a, sc
}

// Resolving a secret must teach the scrubber about it in the same call.
// Otherwise devbay hands an application a credential it can no longer
// recognise in that application's own logs -- which is exactly where
// credentials leak.
func TestResolvingRegistersWithTheScrubber(t *testing.T) {
	b, _, sc := newBroker(t)
	b.Add(&fake{name: "test", value: "sk_test_supersecretvalue"})

	got, err := b.Resolve(context.Background(), "bay1", "stripe/test")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk_test_supersecretvalue" {
		t.Fatalf("value = %q", got)
	}
	scrubbed := sc.String("connecting with sk_test_supersecretvalue")
	if strings.Contains(scrubbed, "supersecretvalue") {
		t.Errorf("the resolved value is not scrubbed: %q", scrubbed)
	}
}

// Everything minted is destroyed when the bay is. A credential that outlives
// its bay is the same class of bug as a container that survives teardown.
func TestMintedCredentialsAreRevokedOnTeardown(t *testing.T) {
	b, audit, _ := newBroker(t)
	revoked := 0
	b.Add(&fake{name: "minter", value: "tok", minted: true, revoked: &revoked,
		expires: time.Now().Add(time.Hour)})

	ctx := context.Background()
	if _, err := b.Resolve(ctx, "bay1", "github"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resolve(ctx, "bay1", "github/2"); err != nil {
		t.Fatal(err)
	}
	if len(b.Grants("bay1")) != 2 {
		t.Fatalf("expected 2 grants, got %d", len(b.Grants("bay1")))
	}

	if err := b.Revoke(ctx, "bay1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revoked != 2 {
		t.Errorf("revoked %d credentials, want 2", revoked)
	}
	if len(b.Grants("bay1")) != 0 {
		t.Error("grants survived revocation")
	}

	events, err := audit.Events()
	if err != nil {
		t.Fatal(err)
	}
	var grants, revokes int
	for _, e := range events {
		switch e.Action {
		case "grant":
			grants++
		case "revoke":
			revokes++
		}
	}
	if grants != 2 || revokes != 2 {
		t.Errorf("audit recorded %d grants and %d revocations, want 2 and 2", grants, revokes)
	}
}

// One credential failing to revoke must not prevent the others, or a single
// expired token strands everything issued after it.
func TestOneFailedRevocationDoesNotStopTheRest(t *testing.T) {
	b, _, _ := newBroker(t)
	ok := 0
	b.Add(&fake{name: "bad", prefix: "bad", value: "x", minted: true,
		revokeErr: errors.New("gone")})
	b.Add(&fake{name: "good", prefix: "good", value: "y", minted: true, revoked: &ok})

	ctx := context.Background()
	for _, ref := range []string{"bad/1", "good/1", "good/2"} {
		if _, err := b.Resolve(ctx, "bay1", ref); err != nil {
			t.Fatal(err)
		}
	}
	err := b.Revoke(ctx, "bay1")
	if err == nil {
		t.Error("a failed revocation should be reported")
	}
	if ok != 2 {
		t.Errorf("revoked %d of the healthy credentials, want 2", ok)
	}
}

// Revocation is scoped to one bay: destroying one must not disarm another.
func TestRevocationIsScopedToOneBay(t *testing.T) {
	b, _, _ := newBroker(t)
	revoked := 0
	b.Add(&fake{name: "m", value: "tok", minted: true, revoked: &revoked})

	ctx := context.Background()
	for _, bay := range []string{"alpha", "beta"} {
		if _, err := b.Resolve(ctx, bay, "github"); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Revoke(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Errorf("revoked %d, want only alpha's", revoked)
	}
	if len(b.Grants("beta")) != 1 {
		t.Error("beta's credential was revoked with alpha's")
	}
}

// A source that claims a reference and then fails is reported rather than
// skipped: falling through to a weaker source would silently downgrade the
// credential without anyone noticing.
func TestAClaimingSourceThatFailsIsNotSkipped(t *testing.T) {
	b, _, _ := newBroker(t)
	b.Add(&fake{name: "primary", prefix: "aws", err: errors.New("session expired")})
	b.Add(&fake{name: "fallback", value: "weaker-value"})

	_, err := b.Resolve(context.Background(), "bay1", "aws/creds")
	if err == nil {
		t.Fatal("expected the failure to surface")
	}
	if !strings.Contains(err.Error(), "session expired") {
		t.Errorf("the underlying cause is lost: %v", err)
	}
}

// Sources are consulted in order, so a specific one can shadow a general one.
func TestSourcesAreTriedInOrder(t *testing.T) {
	b, _, _ := newBroker(t)
	b.Add(&fake{name: "specific", prefix: "github", value: "from-specific"})
	b.Add(&fake{name: "general", value: "from-general"})

	got, err := b.Resolve(context.Background(), "b", "github/1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-specific" {
		t.Errorf("value = %q, want the specific source's", got)
	}

	got, err = b.Resolve(context.Background(), "b", "other/1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-general" {
		t.Errorf("value = %q, want the general source's", got)
	}
}

func TestUnresolvableReferenceIsAnError(t *testing.T) {
	b, _, _ := newBroker(t)
	_, err := b.Resolve(context.Background(), "b", "nothing/here")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestEnvSource(t *testing.T) {
	t.Setenv("DEVBAY_SECRET_STRIPE_TEST", "sk_test_from_env")
	b, _, _ := newBroker(t)
	b.Add(EnvSource{})

	got, err := b.Resolve(context.Background(), "b", "stripe/test")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk_test_from_env" {
		t.Errorf("value = %q", got)
	}
	if EnvName("stripe/test") != "DEVBAY_SECRET_STRIPE_TEST" {
		t.Errorf("EnvName = %q", EnvName("stripe/test"))
	}
}

// The integration with every `<tool> run --` secret manager.
func TestCommandSource(t *testing.T) {
	b, _, _ := newBroker(t)
	// echo stands in for `op read op://{ref}`; the point is the substitution
	// and the trimming, not the tool.
	b.Add(CommandSource{Label: "op", Argv: []string{"echo", "value-for-{ref}"}})

	got, err := b.Resolve(context.Background(), "b", "vault/item")
	if err != nil {
		t.Fatal(err)
	}
	// A trailing newline smuggled into an API key produces a baffling 401.
	if got != "value-for-vault/item" {
		t.Errorf("value = %q; the reference should be substituted and the newline trimmed", got)
	}
}

func TestCommandSourceReportsFailure(t *testing.T) {
	b, _, _ := newBroker(t)
	b.Add(CommandSource{Label: "broken", Argv: []string{"sh", "-c", "echo nope >&2; exit 1"}})

	_, err := b.Resolve(context.Background(), "b", "x")
	if err == nil {
		t.Fatal("a failing command should be an error")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("stderr should reach the caller: %v", err)
	}
}

func TestCommandSourceRespectsPrefix(t *testing.T) {
	s := CommandSource{Prefix: "op/", Argv: []string{"true"}}
	if !s.Handles("op/vault/item") {
		t.Error("should handle its own prefix")
	}
	if s.Handles("aws/creds") {
		t.Error("should not claim another prefix")
	}
}

// ---------------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------------

// The log exists so a developer can answer "what was this given, and when".
// A log that answered it by storing the credentials would be a worse leak
// than the one it was meant to detect.
func TestAuditNeverRecordsValues(t *testing.T) {
	b, audit, _ := newBroker(t)
	const secret = "sk_live_thisMustNeverBeWritten"
	b.Add(&fake{name: "s", value: secret})

	if _, err := b.Resolve(context.Background(), "bay1", "stripe/live"); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(audit.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("the audit log contains a credential:\n%s", body)
	}
	// It must still be useful: the reference and the bay have to be there.
	if !strings.Contains(string(body), "stripe/live") || !strings.Contains(string(body), "bay1") {
		t.Errorf("the log is missing what it exists to record:\n%s", body)
	}
}

func TestAuditIsAppendOnlyAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	first, err := OpenAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Record(Event{Action: "grant", Ref: "a"}); err != nil {
		t.Fatal(err)
	}

	// Reopening must not truncate: a second devbay process, or the same one
	// after a restart, has to add to the record rather than replace it.
	second, err := OpenAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Record(Event{Action: "grant", Ref: "b"}); err != nil {
		t.Fatal(err)
	}

	events, err := second.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want both: %+v", len(events), events)
	}
	if events[0].Ref != "a" || events[1].Ref != "b" {
		t.Errorf("events are out of order: %+v", events)
	}
}

// A crash mid-write leaves a partial final line, which must not make the whole
// log unreadable.
func TestTruncatedFinalLineDoesNotBreakTheLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := OpenAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Record(Event{Action: "grant", Ref: "good"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"action":"grant","ref":"trunc`)
	f.Close()

	events, err := a.Events()
	if err != nil {
		t.Fatalf("a truncated line made the log unreadable: %v", err)
	}
	if len(events) != 1 || events[0].Ref != "good" {
		t.Errorf("expected the intact event to survive, got %+v", events)
	}
}

func TestAuditFileIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if _, err := OpenAudit(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// It records which credentials went where, which is reconnaissance even
	// though it is not secret.
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("audit log mode is %v; it should not be readable by others", fi.Mode().Perm())
	}
}

func TestConcurrentAuditWritesDoNotInterleave(t *testing.T) {
	a, err := OpenAudit(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Record(Event{Action: "grant", Ref: "concurrent"})
		}()
	}
	wg.Wait()

	events, err := a.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 50 {
		t.Errorf("got %d events, want 50; lines were lost or spliced", len(events))
	}
}

// ---------------------------------------------------------------------------
// GitHub App
// ---------------------------------------------------------------------------

func testKey(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), key
}

// The whole GitHub lifecycle against a stand-in API: the assertion is signed
// correctly, the token is scoped, and destroying the bay destroys the token.
func TestGitHubAppMintsScopedTokenAndRevokesIt(t *testing.T) {
	pemBytes, key := testKey(t)

	var (
		mu        sync.Mutex
		gotJWT    string
		gotBody   map[string]any
		revokedTo string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			gotJWT = auth
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_mintedtoken1234567890",
				"expires_at": time.Now().Add(time.Hour).UTC(),
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			revokedTo = auth
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	g := &GitHubApp{
		AppID:          "12345",
		PrivateKeyPEM:  pemBytes,
		InstallationID: "999",
		Repositories:   []string{"acme/api"},
		Permissions:    map[string]string{"contents": "read"},
		BaseURL:        srv.URL,
		HTTP:           srv.Client(),
	}

	b, audit, sc := newBroker(t)
	b.Add(g)

	ctx := context.Background()
	token, err := b.Resolve(ctx, "bay1", "github")
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if token != "ghs_mintedtoken1234567890" {
		t.Errorf("token = %q", token)
	}

	// The token must be scoped to what was asked for, not to everything the
	// installation can see.
	mu.Lock()
	body := gotBody
	jwt := gotJWT
	mu.Unlock()
	if repos, _ := body["repositories"].([]any); len(repos) != 1 || repos[0] != "acme/api" {
		t.Errorf("token was not scoped to the repository: %v", body["repositories"])
	}
	if perms, _ := body["permissions"].(map[string]any); perms["contents"] != "read" {
		t.Errorf("permissions were not narrowed: %v", body["permissions"])
	}

	// The assertion must actually verify against the app's public key.
	verifyJWT(t, jwt, &key.PublicKey, "12345")

	// A minted token is scrubbed like any other secret.
	if strings.Contains(sc.String("using ghs_mintedtoken1234567890"), "mintedtoken") {
		t.Error("the minted token is not scrubbed")
	}

	// And destroying the bay destroys the token.
	if err := b.Revoke(ctx, "bay1"); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	mu.Lock()
	got := revokedTo
	mu.Unlock()
	if got != "ghs_mintedtoken1234567890" {
		t.Errorf("revocation authenticated with %q, want the minted token", got)
	}

	events, _ := audit.Events()
	var sawMinted bool
	for _, e := range events {
		if e.Action == "grant" && e.Minted && !e.ExpiresAt.IsZero() {
			sawMinted = true
		}
	}
	if !sawMinted {
		t.Errorf("the audit should record the grant as minted with an expiry: %+v", events)
	}
}

// verifyJWT checks the assertion the way GitHub would.
func verifyJWT(t *testing.T, token string, pub *rsa.PublicKey, wantIssuer string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("the assertion is not a JWT: %q", token)
	}
	enc := base64.RawURLEncoding
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature is not base64url: %v", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("the assertion does not verify against the app key: %v", err)
	}

	raw, err := enc.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != wantIssuer {
		t.Errorf("issuer = %v, want %s", claims["iss"], wantIssuer)
	}
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	now := float64(time.Now().Unix())
	// Backdated, because a few seconds of clock skew against GitHub produces a
	// bewildering 401 otherwise.
	if iat > now {
		t.Errorf("iat is in the future; clock skew will reject this")
	}
	// GitHub caps the lifetime at ten minutes and rejects more.
	if exp-iat > 600 {
		t.Errorf("the assertion lives %.0fs, which GitHub rejects", exp-iat)
	}
}

func TestGitHubAppRevocationToleratesAnAlreadyDeadToken(t *testing.T) {
	pemBytes, _ := testKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			// Already expired, which is what teardown finds after a long gap.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now()})
	}))
	defer srv.Close()

	g := &GitHubApp{AppID: "1", PrivateKeyPEM: pemBytes, InstallationID: "2",
		BaseURL: srv.URL, HTTP: srv.Client()}
	b, _, _ := newBroker(t)
	b.Add(g)

	ctx := context.Background()
	if _, err := b.Resolve(ctx, "bay1", "github"); err != nil {
		t.Fatal(err)
	}
	if err := b.Revoke(ctx, "bay1"); err != nil {
		t.Errorf("an already-expired token should not fail teardown: %v", err)
	}
}

func TestGitHubAppReportsMintingFailure(t *testing.T) {
	pemBytes, _ := testKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"message": "Integration not found"})
	}))
	defer srv.Close()

	g := &GitHubApp{AppID: "1", PrivateKeyPEM: pemBytes, InstallationID: "2",
		BaseURL: srv.URL, HTTP: srv.Client()}
	b, _, _ := newBroker(t)
	b.Add(g)

	_, err := b.Resolve(context.Background(), "bay1", "github")
	if err == nil {
		t.Fatal("a failed mint should be an error")
	}
	if !strings.Contains(err.Error(), "Integration not found") {
		t.Errorf("GitHub's own message should reach the caller: %v", err)
	}
}

func TestGitHubAppNeedsAnInstallation(t *testing.T) {
	pemBytes, _ := testKey(t)
	g := &GitHubApp{AppID: "1", PrivateKeyPEM: pemBytes}
	_, _, err := g.Resolve(context.Background(), "b", "github")
	if err == nil || !strings.Contains(err.Error(), "installation") {
		t.Errorf("err = %v, want a message naming the missing installation id", err)
	}
}

func TestGitHubAppAcceptsBothKeyEncodings(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	// GitHub has handed out both over the years, and a developer should not
	// have to know which one they were given.
	for name, body := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		if _, err := parseRSAKey(body); err != nil {
			t.Errorf("%s key rejected: %v", name, err)
		}
	}
	if _, err := parseRSAKey([]byte("not a key")); err == nil {
		t.Error("a non-PEM key should be rejected")
	}
}

func TestGitHubAppHandles(t *testing.T) {
	g := &GitHubApp{}
	for ref, want := range map[string]bool{
		"github": true, "github/12345": true,
		"stripe/test": false, "githubbish": false,
	} {
		if got := g.Handles(ref); got != want {
			t.Errorf("Handles(%q) = %v, want %v", ref, got, want)
		}
	}
}
