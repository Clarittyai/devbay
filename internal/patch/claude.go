// Package patch repairs a devbay manifest using Claude.
//
// This is the one place in devbay where a model runs. It sits behind
// verify.Patcher, which is the only seam the execution plane exposes to the
// authoring plane, and it is deliberately small: given the manifest that just
// failed and the evidence of how it failed, return a revised manifest.
//
// # Why a model at all
//
// The deterministic detector reaches a useful majority of repositories and
// stops. What it cannot do is read an error message. "connection refused on
// 5432" plus "the api service starts before db is ready" is a two-line fix
// that no rule table will ever contain, because the space of ways an
// application can fail to start is not enumerable. That is the job here, and
// it is the whole job -- this package proposes text, and nothing else.
//
// # What it is not trusted with
//
// Everything it returns crosses verify's airlock before anything executes it:
// strict parse, full validation, and unconditional removal of `egress:`. That
// ordering is the design. A patcher reads the error output of code from the
// repository, which is exactly the material an attacker can influence, so the
// question is never whether the model can be persuaded to write something
// hostile -- it is whether writing it would achieve anything. It does not.
//
// # What never reaches it
//
// HC1: a secret must not enter model context. Container logs are the most
// likely carrier, since an application will happily print its own
// configuration, so every byte of evidence passes through a scrubber before it
// is put in a message -- known values first, credential shapes second.
//
// # Local-first
//
// devbay makes no network calls except image registries, a manifest's declared
// egress, and this. The API key lives in the daemon on the host and is never
// injected into a bay, and the model is off unless the developer turns it on.
package patch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Clarittyai/devbay/internal/scrub"
	"github.com/Clarittyai/devbay/internal/verify"
)

// Defaults. The model is the current Opus because this is a correctness task
// on a small input where a wrong answer costs a boot cycle and a confused
// developer; effort is high rather than xhigh because the task is narrow --
// read one failure, change a few lines -- and the extra depth buys nothing an
// extra attempt would not.
const (
	DefaultModel     = anthropic.ModelClaudeOpus5
	DefaultEffort    = anthropic.OutputConfigEffortHigh
	DefaultMaxTokens = 16000

	// maxEvidence bounds each piece of evidence handed to the model. A crash
	// loop can produce megabytes of identical lines, and the useful part is
	// always the end.
	maxEvidence = 8 << 10
)

// Claude proposes manifest revisions.
//
// The zero value is not usable; call New.
type Claude struct {
	client anthropic.Client

	// Model, Effort and MaxTokens are the request knobs, defaulted by New.
	Model     string
	Effort    anthropic.OutputConfigEffort
	MaxTokens int64

	// Scrub removes known secret values from evidence. Shape-based scrubbing
	// happens regardless; this adds the values the broker actually resolved,
	// which is the one thing a pattern matcher cannot know.
	Scrub *scrub.Scrubber

	// Log receives one line per attempt, including the model's own account of
	// what it changed. The developer is going to read the resulting file, so
	// they should be able to see how it got that way.
	Log func(format string, args ...any)

	// calls counts requests, for tests and for bounding.
	calls int
}

// Option configures a Claude patcher.
type Option func(*Claude) []option.RequestOption

// WithModel overrides the model.
func WithModel(m string) Option {
	return func(c *Claude) []option.RequestOption { c.Model = m; return nil }
}

// WithEffort overrides the effort level.
func WithEffort(e anthropic.OutputConfigEffort) Option {
	return func(c *Claude) []option.RequestOption { c.Effort = e; return nil }
}

// WithScrubber attaches the broker's scrubber, so values devbay itself handed
// to the containers are removed from evidence by value rather than by shape.
func WithScrubber(s *scrub.Scrubber) Option {
	return func(c *Claude) []option.RequestOption { c.Scrub = s; return nil }
}

// WithLog attaches a progress logger.
func WithLog(f func(string, ...any)) Option {
	return func(c *Claude) []option.RequestOption { c.Log = f; return nil }
}

// WithAPIKey sets the key explicitly rather than taking it from the
// environment. Used by tests.
func WithAPIKey(k string) Option {
	return func(*Claude) []option.RequestOption { return []option.RequestOption{option.WithAPIKey(k)} }
}

// WithBaseURL points the client at another endpoint. Used by tests.
func WithBaseURL(u string) Option {
	return func(*Claude) []option.RequestOption { return []option.RequestOption{option.WithBaseURL(u)} }
}

// New builds a patcher.
func New(opts ...Option) *Claude {
	c := &Claude{
		Model:     DefaultModel,
		Effort:    DefaultEffort,
		MaxTokens: DefaultMaxTokens,
	}
	var req []option.RequestOption
	for _, o := range opts {
		req = append(req, o(c)...)
	}
	c.client = anthropic.NewClient(req...)
	return c
}

// ErrDisabled is returned by FromEnv when no model is configured.
var ErrDisabled = errors.New("no model is configured")

// FromEnv builds a patcher when the developer has asked for one, and returns
// ErrDisabled otherwise.
//
// Opt-in rather than automatic. `devbay init` on a fresh repository should not
// silently make a network call to a third party and bill someone for it; a
// tool that runs entirely on your machine except for the times it doesn't is
// not a tool that runs entirely on your machine. DEVBAY_NO_MODEL turns it off
// again for a CI run or an airgapped machine that happens to have a key in the
// environment.
func FromEnv(opts ...Option) (*Claude, error) {
	if os.Getenv("DEVBAY_NO_MODEL") != "" {
		return nil, ErrDisabled
	}
	model := os.Getenv("DEVBAY_MODEL")
	if os.Getenv("ANTHROPIC_API_KEY") == "" && model == "" {
		return nil, ErrDisabled
	}
	if model != "" {
		opts = append(opts, WithModel(model))
	}
	return New(opts...), nil
}

// Calls reports how many requests have been made.
func (c *Claude) Calls() int { return c.calls }

// proposal is the shape the model must return.
//
// Constrained by schema rather than asked for politely: a patcher that returns
// prose with the YAML somewhere inside it turns every attempt into a parsing
// problem, and a fenced block is not a contract.
var proposalSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"manifest": map[string]any{
			"type":        "string",
			"description": "The complete revised devbay.yaml. Not a diff, not a fragment: the whole file, as it should be written to disk.",
		},
		"change": map[string]any{
			"type":        "string",
			"description": "One sentence naming what was changed and which part of the evidence prompted it.",
		},
		"confident": map[string]any{
			"type":        "boolean",
			"description": "False when the evidence does not identify the cause and the revision is a guess. Say so rather than presenting a guess as a fix.",
		},
	},
	"required":             []string{"manifest", "change", "confident"},
	"additionalProperties": false,
}

type proposal struct {
	Manifest  string `json:"manifest"`
	Change    string `json:"change"`
	Confident bool   `json:"confident"`
}

// brief is the stable half of the prompt: the format's shape and the rules a
// proposal has to satisfy. It is stated once, plainly. The validator's own
// error messages carry the precise rule when one is broken and are fed back
// through Failure, so this does not try to be the schema -- it is the context
// that lets a first attempt land, and the feedback supplies the rest.
//
// Kept byte-stable and placed first so it caches: the verify loop makes up to
// three calls with this identical prefix, and every repository on the machine
// makes more.
const brief = `You repair devbay manifests.

A devbay.yaml describes how to run one repository's services and tasks in
isolated local containers. A developer or an agent is about to execute it. You
are given a manifest that did not work and the evidence of how it failed, and
you return a corrected manifest.

The format:

  version: 1
  project: <slug>
  services:
    <name>:
      image: <ref>          # required
      scope: bay | shared   # per-bay by default
      install: [argv, ...]  # optional
      start:   [argv, ...]  # required unless the image is a server that starts itself
      port: <int>           # the port the process listens on inside the container
      primary: true         # exactly one service, the one a browser should open
      needs: [<service>, ...]
      health: {http: /path} | {tcp: <port>} | {cmd: [argv, ...]} | {log: <substring>}
      env:
        KEY: value | ${bay.<svc>.url} | ${bay.<svc>.public_url} | ${secret:<ref>}
      volumes: [node_modules, ...]   # dependency dirs, kept out of the bind mount
      watch: ["src/**"]              # host-side; triggers a container action
  tasks:
    <name>: {run: [argv, ...], needs: [<service>, ...]}

The rules that will reject a proposal:

  - Commands are argv arrays. [pnpm, dev], never "pnpm dev". There is no way to
    express a shell command, a pipeline, or a substitution in this format. If a
    repository genuinely needs one, point argv at the script in the repository:
    [bash, scripts/setup.sh].
  - argv[0] is drawn from a known set of interpreters and package managers.
    Anything else requires the developer to approve it once, so prefer the
    conventional entry point.
  - Environment values are literals or references. A credential literal is
    rejected outright; write ${secret:<ref>}.
  - Every service declares health. Without a probe there is nothing to verify
    and devbay is guessing about whether the service came up.
  - Every task declares needs, even when empty. needs: [] means "boots nothing",
    which is what makes a unit test fast; omitting the key is an error rather
    than a default, because the default would silently be wrong.
  - Do not write egress:. Network policy is not yours to set and is removed
    from anything you return.

The three address planes, which is where most env mistakes live:

  - ${bay.<svc>.url} is how one container reaches another, and how the host
    reaches a service. Server-side variables want this.
  - ${bay.<svc>.public_url} is the browser origin. Anything a browser will read
    -- NEXT_PUBLIC_*, VITE_*, an OAuth redirect -- wants this. Using url here
    produces the classic "works in the browser, breaks in SSR" failure, and its
    mirror image.

How to read the evidence:

  - stage: parse means the YAML is malformed or a command is a string.
  - stage: validate means it parsed and broke a rule; the message names it.
  - stage: boot means it started and did not become healthy. The logs are the
    container's own output and are usually where the real cause is: a missing
    variable, a refused connection, a migration that has not run.

Change what the evidence points at. A manifest that fails on the database is
not evidence that the web service is wrong, and rewriting both makes the next
failure harder to read. If the evidence does not identify a cause, make the
smallest plausible change and set confident to false.`

// Patch implements verify.Patcher.
func (c *Claude) Patch(ctx context.Context, current []byte, f verify.Failure) ([]byte, error) {
	c.calls++

	msg, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     c.Model,
		MaxTokens: c.MaxTokens,
		System: []anthropic.TextBlockParam{{
			Text: brief,
			// The prefix is identical across every attempt and every
			// repository, and the loop makes up to three calls per run.
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: c.Effort,
			Format: anthropic.JSONOutputFormatParam{Schema: proposalSchema},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(c.evidence(current, f))),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("asking %s for a revision: %w", c.Model, err)
	}

	// A refusal is a successful response with an empty body, so reading
	// content[0] first would produce a confusing index panic instead of the
	// actual reason.
	if msg.StopReason == anthropic.StopReasonRefusal {
		return nil, fmt.Errorf("the request was declined (%s); the repository's own content may be triggering it",
			msg.StopDetails.Category)
	}

	var text strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	if text.Len() == 0 {
		return nil, fmt.Errorf("no revision was returned (stop reason %q)", msg.StopReason)
	}

	var p proposal
	if err := json.Unmarshal([]byte(text.String()), &p); err != nil {
		return nil, fmt.Errorf("the revision was not in the requested form: %w", err)
	}
	if strings.TrimSpace(p.Manifest) == "" {
		return nil, errors.New("an empty manifest was returned")
	}

	if c.Log != nil {
		note := p.Change
		if !p.Confident {
			note += " (uncertain: the evidence does not identify the cause)"
		}
		c.Log("  %s", note)
	}

	out := p.Manifest
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return []byte(out), nil
}

// evidence renders the failure into the message body, scrubbed.
func (c *Claude) evidence(current []byte, f verify.Failure) string {
	var b strings.Builder
	b.WriteString("This manifest did not work:\n\n```yaml\n")
	b.WriteString(strings.TrimRight(string(current), "\n"))
	b.WriteString("\n```\n\nWhat happened:\n\n")
	fmt.Fprintf(&b, "  stage:   %s\n", f.Stage)
	if f.Service != "" {
		fmt.Fprintf(&b, "  service: %s\n", f.Service)
	}
	fmt.Fprintf(&b, "  error:   %s\n", c.clean(f.Message))
	if logs := c.clean(f.Logs); logs != "" {
		b.WriteString("\nThe container printed:\n\n```\n")
		b.WriteString(tail(logs, maxEvidence))
		b.WriteString("\n```\n")
	}
	b.WriteString("\nReturn the corrected manifest in full.")
	return b.String()
}

// clean removes secrets from a piece of evidence: values devbay knows it
// handed out, then anything credential-shaped that it did not.
func (c *Claude) clean(s string) string {
	if s == "" {
		return ""
	}
	if c.Scrub != nil {
		s = c.Scrub.String(s)
	} else {
		s = scrub.Text(s)
	}
	return strings.TrimSpace(s)
}

// tail keeps the last n bytes, on a line boundary. A crash loop repeats
// itself, and the end is where the cause is.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return "[…earlier output omitted…]\n" + s
}

var _ verify.Patcher = (*Claude)(nil)
