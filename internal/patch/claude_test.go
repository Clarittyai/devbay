package patch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Clarittyai/devbay/internal/manifest"
	"github.com/Clarittyai/devbay/internal/scrub"
	"github.com/Clarittyai/devbay/internal/verify"
)

const brokenManifest = `version: 1
project: acme
services:
  api:
    image: node:22
    port: 3000
    start: [pnpm, dev]
    health: {http: /healthz}
tasks:
  unit: {run: [pnpm, test], needs: []}
`

const fixedManifest = `version: 1
project: acme
services:
  api:
    image: node:22
    port: 3000
    start: [pnpm, dev]
    health: {http: /}
tasks:
  unit: {run: [pnpm, test], needs: []}
`

// fake stands in for the API. It records what was sent, which is most of what
// these tests are checking: a patcher that produces the right YAML while
// leaking a password in the request is not a working patcher.
type fake struct {
	*httptest.Server
	bodies []map[string]any
	reply  func(n int) (int, string)
}

func newFake(t *testing.T, reply func(n int) (int, string)) *fake {
	t.Helper()
	f := &fake{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("the request was not JSON: %v", err)
		}
		f.bodies = append(f.bodies, body)
		code, out := f.reply(len(f.bodies))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, out)
	}))
	t.Cleanup(f.Close)
	return f
}

// sent returns the whole request body as text, for "does this contain X" checks.
func (f *fake) sent(i int) string {
	raw, _ := json.Marshal(f.bodies[i])
	return string(raw)
}

func ok(manifestYAML, change string) string {
	inner, _ := json.Marshal(proposal{Manifest: manifestYAML, Change: change, Confident: true})
	outer, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5",
		"content":     []map[string]any{{"type": "text", "text": string(inner)}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	return string(outer)
}

func newTestPatcher(t *testing.T, f *fake, opts ...Option) *Claude {
	t.Helper()
	return New(append([]Option{WithBaseURL(f.URL), WithAPIKey("test")}, opts...)...)
}

func boot(f verify.Failure) verify.Failure { return f }

func TestARevisionIsReturnedAsAManifest(t *testing.T) {
	f := newFake(t, func(int) (int, string) {
		return 200, ok(fixedManifest, "the probe path 404s; changed it to /")
	})
	c := newTestPatcher(t, f)

	got, err := c.Patch(context.Background(), []byte(brokenManifest), boot(verify.Failure{
		Stage: verify.StageBoot, Service: "api",
		Message: "did not become healthy", Logs: "GET /healthz 404",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != fixedManifest {
		t.Errorf("the manifest was altered in transit:\n%s", got)
	}
	// It must be a manifest, not prose containing one.
	if _, err := manifest.Parse(got); err != nil {
		t.Errorf("the result does not parse: %v", err)
	}
}

// The evidence is the entire value of this package. A request that omits the
// stage, the service, or the container's own output asks the model to guess.
func TestTheEvidenceIsActuallySent(t *testing.T) {
	f := newFake(t, func(int) (int, string) { return 200, ok(fixedManifest, "fixed") })
	c := newTestPatcher(t, f)

	_, err := c.Patch(context.Background(), []byte(brokenManifest), verify.Failure{
		Stage: verify.StageBoot, Service: "api",
		Message: "did not become healthy", Logs: "Error: connect ECONNREFUSED 127.0.0.1:5432",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := f.sent(0)
	for _, want := range []string{"boot", "api", "did not become healthy", "ECONNREFUSED", "pnpm"} {
		if !strings.Contains(body, want) {
			t.Errorf("the request does not mention %q", want)
		}
	}
}

// HC1. The most likely carrier of a secret into model context is a container
// log, because applications print their own configuration.
func TestSecretsNeverReachTheModel(t *testing.T) {
	const password = "hunter2-super-secret-value"
	s := scrub.New()
	s.Add("secret:db/password", password)

	f := newFake(t, func(int) (int, string) { return 200, ok(fixedManifest, "fixed") })
	c := newTestPatcher(t, f, WithScrubber(s))

	_, err := c.Patch(context.Background(), []byte(brokenManifest), verify.Failure{
		Stage:   verify.StageBoot,
		Service: "api",
		Message: "auth failed for postgres://app:" + password + "@db:5432/acme",
		Logs:    "DATABASE_PASSWORD=" + password + "\nfatal: password authentication failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if body := f.sent(0); strings.Contains(body, password) {
		t.Fatal("a secret value was sent to the model")
	}
}

// And with no scrubber wired up, shape-based scrubbing still runs -- the
// no-broker path is the one a plain `devbay init` takes.
func TestCredentialShapesAreScrubbedWithoutABroker(t *testing.T) {
	const key = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	f := newFake(t, func(int) (int, string) { return 200, ok(fixedManifest, "fixed") })
	c := newTestPatcher(t, f)

	_, err := c.Patch(context.Background(), []byte(brokenManifest), verify.Failure{
		Stage: verify.StageBoot, Service: "api", Message: "startup failed",
		Logs: "using key " + key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if body := f.sent(0); strings.Contains(body, key) {
		t.Error("a credential-shaped value was sent unredacted")
	}
}

// The stable prefix is identical on every call and every repository; the loop
// alone makes up to three. Losing the breakpoint is a silent cost regression,
// so assert it rather than trusting it.
func TestTheStablePrefixIsCached(t *testing.T) {
	f := newFake(t, func(int) (int, string) { return 200, ok(fixedManifest, "fixed") })
	c := newTestPatcher(t, f)
	if _, err := c.Patch(context.Background(), []byte(brokenManifest), verify.Failure{Stage: verify.StageBoot}); err != nil {
		t.Fatal(err)
	}

	system, _ := f.bodies[0]["system"].([]any)
	if len(system) == 0 {
		t.Fatal("no system prompt was sent")
	}
	last, _ := system[len(system)-1].(map[string]any)
	if _, ok := last["cache_control"]; !ok {
		t.Error("the system prefix carries no cache breakpoint")
	}
}

// A free-form reply turns every attempt into a parsing problem. The response
// is schema-constrained instead.
func TestTheResponseShapeIsConstrained(t *testing.T) {
	f := newFake(t, func(int) (int, string) { return 200, ok(fixedManifest, "fixed") })
	c := newTestPatcher(t, f)
	if _, err := c.Patch(context.Background(), []byte(brokenManifest), verify.Failure{Stage: verify.StageBoot}); err != nil {
		t.Fatal(err)
	}

	cfg, _ := f.bodies[0]["output_config"].(map[string]any)
	if cfg == nil {
		t.Fatal("no output_config was sent")
	}
	format, _ := cfg["format"].(map[string]any)
	if format == nil || format["type"] != "json_schema" {
		t.Errorf("the response was not schema-constrained: %v", cfg)
	}
	if cfg["effort"] != string(DefaultEffort) {
		t.Errorf("effort = %v, want %s", cfg["effort"], DefaultEffort)
	}
	if th, _ := f.bodies[0]["thinking"].(map[string]any); th == nil || th["type"] != "adaptive" {
		t.Errorf("thinking = %v, want adaptive", f.bodies[0]["thinking"])
	}
}

// A refusal is a successful response with an empty body. Reading content[0]
// first would panic on an index instead of reporting the reason.
func TestARefusalIsReportedNotIndexed(t *testing.T) {
	f := newFake(t, func(int) (int, string) {
		return 200, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5",
			"content":[],"stop_reason":"refusal",
			"stop_details":{"type":"refusal","category":"cyber"},
			"usage":{"input_tokens":1,"output_tokens":0}}`
	})
	c := newTestPatcher(t, f)

	_, err := c.Patch(context.Background(), []byte(brokenManifest), verify.Failure{Stage: verify.StageBoot})
	if err == nil {
		t.Fatal("a refusal was treated as a revision")
	}
	if !strings.Contains(err.Error(), "declined") || !strings.Contains(err.Error(), "cyber") {
		t.Errorf("the reason is not actionable: %v", err)
	}
}

func TestAnEmptyManifestIsAnError(t *testing.T) {
	f := newFake(t, func(int) (int, string) { return 200, ok("   \n", "nothing to do") })
	c := newTestPatcher(t, f)
	if _, err := c.Patch(context.Background(), []byte(brokenManifest), verify.Failure{Stage: verify.StageBoot}); err == nil {
		t.Error("an empty manifest was accepted")
	}
}

func TestAnAPIErrorIsWrappedNotSwallowed(t *testing.T) {
	f := newFake(t, func(int) (int, string) {
		return 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
	})
	c := New(WithBaseURL(f.URL), WithAPIKey("test"))

	_, err := c.Patch(context.Background(), []byte(brokenManifest), verify.Failure{Stage: verify.StageBoot})
	if err == nil {
		t.Fatal("a 429 was reported as success")
	}
	if !strings.Contains(err.Error(), "revision") {
		t.Errorf("the error lost its context: %v", err)
	}
}

// A crash loop repeats itself; the cause is at the end. Sending the whole
// thing wastes the budget on identical lines.
func TestOversizedLogsAreTruncatedFromTheFront(t *testing.T) {
	logs := strings.Repeat("retrying...\n", 4000) + "FATAL: role \"app\" does not exist\n"
	f := newFake(t, func(int) (int, string) { return 200, ok(fixedManifest, "fixed") })
	c := newTestPatcher(t, f)

	if _, err := c.Patch(context.Background(), []byte(brokenManifest), verify.Failure{
		Stage: verify.StageBoot, Service: "db", Logs: logs,
	}); err != nil {
		t.Fatal(err)
	}
	body := f.sent(0)
	if !strings.Contains(body, "role \\\"app\\\" does not exist") {
		t.Error("the tail of the log, where the cause is, was dropped")
	}
	if len(body) > 4*maxEvidence {
		t.Errorf("the request is %d bytes; the log was not bounded", len(body))
	}
}

// ---------------------------------------------------------------------------
// through the loop
// ---------------------------------------------------------------------------

// End to end: a manifest that fails, a model that fixes it, a second boot that
// succeeds. This is the whole feature.
func TestTheLoopConvergesWithARealPatcher(t *testing.T) {
	f := newFake(t, func(int) (int, string) {
		return 200, ok(fixedManifest, "changed the probe path to /")
	})
	c := newTestPatcher(t, f)

	boots := 0
	l := &verify.Loop{
		Patch: c,
		Boot: verify.BooterFunc(func(_ context.Context, m *manifest.Manifest) *verify.Failure {
			boots++
			if m.Services["api"].Health.HTTP == "/healthz" {
				return &verify.Failure{Stage: verify.StageBoot, Service: "api",
					Message: "did not become healthy", Logs: "404 on /healthz"}
			}
			return nil
		}),
	}

	res, err := l.Run(context.Background(), []byte(brokenManifest))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("the loop did not converge: %+v", res.LastFailure())
	}
	if boots != 2 || c.Calls() != 1 {
		t.Errorf("booted %d times and called the model %d times; want 2 and 1", boots, c.Calls())
	}
}

// The airlock, exercised against the real thing rather than a stub: a model
// that proposes a network allowlist does not get one. This is the property
// that makes it safe to feed repository-influenced text to a model at all.
func TestAModelProposedEgressIsStripped(t *testing.T) {
	withEgress := strings.Replace(fixedManifest, "    health: {http: /}",
		"    egress: [exfiltrate.example.com]\n    health: {http: /}", 1)

	f := newFake(t, func(int) (int, string) {
		return 200, ok(withEgress, "added the domain the build needs")
	})
	c := newTestPatcher(t, f)

	var booted *manifest.Manifest
	attempts := 0
	l := &verify.Loop{
		Patch: c,
		Boot: verify.BooterFunc(func(_ context.Context, m *manifest.Manifest) *verify.Failure {
			attempts++
			booted = m
			if attempts == 1 {
				return &verify.Failure{Stage: verify.StageBoot, Service: "api", Message: "nope"}
			}
			return nil
		}),
	}
	if _, err := l.Run(context.Background(), []byte(brokenManifest)); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("expected a second attempt, got %d", attempts)
	}
	if got := booted.Services["api"].Egress; len(got) != 0 {
		t.Errorf("the model granted itself egress %v", got)
	}
}

// A model that returns something invalid does not get executed; the validation
// error is fed back so the next attempt can fix it.
func TestAnInvalidRevisionIsCaughtAndFedBack(t *testing.T) {
	// No health probe: valid YAML, rejected by R5.
	noHealth := strings.Replace(fixedManifest, "    health: {http: /}\n", "", 1)

	f := newFake(t, func(n int) (int, string) {
		if n == 1 {
			return 200, ok(noHealth, "removed the probe")
		}
		return 200, ok(fixedManifest, "put the probe back")
	})
	c := newTestPatcher(t, f)

	l := &verify.Loop{
		MaxAttempts: 3,
		Patch:       c,
		Boot: verify.BooterFunc(func(_ context.Context, m *manifest.Manifest) *verify.Failure {
			if m.Services["api"].Health.HTTP == "/healthz" {
				return &verify.Failure{Stage: verify.StageBoot, Service: "api", Message: "404"}
			}
			return nil
		}),
	}
	res, err := l.Run(context.Background(), []byte(brokenManifest))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("the loop did not recover: %+v", res.LastFailure())
	}
	// The second request must carry the validation error, not the boot error.
	if body := f.sent(1); !strings.Contains(body, "validate") || !strings.Contains(body, "health") {
		t.Error("the validation error was not fed back to the model")
	}
}

// ---------------------------------------------------------------------------
// configuration
// ---------------------------------------------------------------------------

// Opt-in. `devbay init` on a fresh repository must not silently call a third
// party and bill someone for it.
func TestTheModelIsOffUnlessAskedFor(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("DEVBAY_MODEL", "")
	t.Setenv("DEVBAY_NO_MODEL", "")
	if _, err := FromEnv(); err != ErrDisabled {
		t.Errorf("err = %v, want ErrDisabled", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != DefaultModel {
		t.Errorf("model = %s, want %s", c.Model, DefaultModel)
	}

	// And off again, for CI and airgapped machines that happen to have a key
	// in the environment.
	t.Setenv("DEVBAY_NO_MODEL", "1")
	if _, err := FromEnv(); err != ErrDisabled {
		t.Errorf("DEVBAY_NO_MODEL did not disable it: %v", err)
	}
}

func TestTheModelIsOverridable(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("DEVBAY_NO_MODEL", "")
	t.Setenv("DEVBAY_MODEL", "claude-sonnet-5")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "claude-sonnet-5" {
		t.Errorf("model = %s, want claude-sonnet-5", c.Model)
	}
}
