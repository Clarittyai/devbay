// Package broker resolves ${secret:...} references and revokes what it minted.
//
// It is deliberately a consumer rather than a store. The ecosystem converged
// on one shape years ago -- `op run --`, `sops exec-env`, `dotenvx run --`,
// `vault read`, `infisical run --` all inject values into a subprocess and
// hold nothing -- so the useful thing to build is something that speaks to all
// of them, not a competitor that asks people to move their secrets again.
//
// What is worth building is the part none of them do: tying a credential's
// lifetime to a bay's. A long-lived key handed to an environment that a coding
// agent drives is a key that outlives every mistake made with it. Where a
// provider can mint a short-lived scoped credential, devbay mints one per bay
// and revokes it when the bay is destroyed.
//
// Three rules hold throughout:
//
//   - A value is fetched when a container is created, never earlier, and is
//     never written to disk by devbay.
//   - Every grant is recorded in an append-only log: which reference, which
//     bay, which provider, when. The value itself is never recorded.
//   - Anything minted is revoked on teardown. A credential that outlives the
//     bay it was issued for is the same class of bug as a leaked container.
package broker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Clarittyai/devbay/internal/scrub"
)

// Grant is one credential issued to one bay.
type Grant struct {
	Ref      string    `json:"ref"`
	Provider string    `json:"provider"`
	Bay      string    `json:"bay"`
	IssuedAt time.Time `json:"issued_at"`
	// ExpiresAt is zero for a credential with no known lifetime, which is
	// itself worth seeing in the log.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// Minted distinguishes a credential devbay created, and must therefore
	// destroy, from one it merely read.
	Minted bool `json:"minted"`

	revoke func(context.Context) error
}

// Expired reports whether a grant has passed its stated lifetime.
func (g *Grant) Expired(now time.Time) bool {
	return !g.ExpiresAt.IsZero() && now.After(g.ExpiresAt)
}

// Source resolves references of a particular shape.
type Source interface {
	// Name identifies the source in the audit log.
	Name() string
	// Handles reports whether this source claims a reference.
	Handles(ref string) bool
	// Resolve returns the value and, when it minted one, a revocable grant.
	Resolve(ctx context.Context, bay, ref string) (string, *Grant, error)
}

// Broker resolves references through an ordered list of sources.
type Broker struct {
	mu      sync.Mutex
	sources []Source
	grants  map[string][]*Grant // by bay

	audit *Audit
	scrub *scrub.Scrubber
	Log   func(format string, args ...any)
}

// New returns a broker. Sources are consulted in order, so the most specific
// should come first.
func New(audit *Audit, sc *scrub.Scrubber, logf func(string, ...any)) *Broker {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Broker{
		grants: map[string][]*Grant{},
		audit:  audit,
		scrub:  sc,
		Log:    logf,
	}
}

// Add registers a source.
func (b *Broker) Add(s Source) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sources = append(b.sources, s)
}

// ErrNotFound means no source could resolve a reference.
var ErrNotFound = errors.New("no source could resolve this secret")

// Resolve fetches a value for a bay.
//
// The value is registered with the scrubber in the same call that produces it.
// Resolving without doing so would hand an application a credential devbay
// could no longer recognise in that application's own logs, which is precisely
// where credentials leak.
func (b *Broker) Resolve(ctx context.Context, bay, ref string) (string, error) {
	b.mu.Lock()
	sources := append([]Source{}, b.sources...)
	b.mu.Unlock()

	for _, s := range sources {
		if !s.Handles(ref) {
			continue
		}
		value, grant, err := s.Resolve(ctx, bay, ref)
		if err != nil {
			// A source that claims a reference and then fails is reported
			// rather than skipped: falling through to a weaker source would
			// silently downgrade the credential.
			return "", fmt.Errorf("broker: %s could not resolve %q: %w", s.Name(), ref, err)
		}
		if value == "" {
			continue
		}

		if b.scrub != nil {
			b.scrub.Add(ref, value)
		}
		if grant == nil {
			grant = &Grant{Ref: ref, Provider: s.Name(), Bay: bay, IssuedAt: time.Now()}
		}
		grant.Ref, grant.Provider, grant.Bay = ref, s.Name(), bay
		if grant.IssuedAt.IsZero() {
			grant.IssuedAt = time.Now()
		}

		b.mu.Lock()
		b.grants[bay] = append(b.grants[bay], grant)
		b.mu.Unlock()

		if b.audit != nil {
			_ = b.audit.Record(Event{
				Action: "grant", Bay: bay, Ref: ref, Provider: s.Name(),
				Minted: grant.Minted, ExpiresAt: grant.ExpiresAt,
			})
		}
		return value, nil
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, ref)
}

// Revoke destroys every credential minted for a bay.
//
// Called from teardown. A credential that outlives the bay it was issued for
// is the same class of bug as a container that survives `devbay rm`: invisible,
// and discovered later by someone else.
func (b *Broker) Revoke(ctx context.Context, bay string) error {
	b.mu.Lock()
	grants := b.grants[bay]
	delete(b.grants, bay)
	b.mu.Unlock()

	var errs []error
	for _, g := range grants {
		if g.revoke == nil {
			continue
		}
		if err := g.revoke(ctx); err != nil {
			errs = append(errs, fmt.Errorf("revoking %s: %w", g.Ref, err))
			if b.audit != nil {
				_ = b.audit.Record(Event{Action: "revoke-failed", Bay: bay, Ref: g.Ref, Provider: g.Provider})
			}
			continue
		}
		b.Log("  revoked %s (%s)", g.Ref, g.Provider)
		if b.audit != nil {
			_ = b.audit.Record(Event{Action: "revoke", Bay: bay, Ref: g.Ref, Provider: g.Provider})
		}
	}
	return errors.Join(errs...)
}

// Grants returns the credentials currently held for a bay, without values.
func (b *Broker) Grants(bay string) []Grant {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Grant, 0, len(b.grants[bay]))
	for _, g := range b.grants[bay] {
		out = append(out, *g)
	}
	return out
}

// Lookup adapts a broker to the resolver's signature.
func (b *Broker) Lookup(bay string) func(string) (string, bool) {
	return func(ref string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		v, err := b.Resolve(ctx, bay, ref)
		if err != nil {
			b.Log("  secret %q unavailable: %v", ref, err)
			return "", false
		}
		return v, true
	}
}

// ---------------------------------------------------------------------------
// env
// ---------------------------------------------------------------------------

// EnvSource reads DEVBAY_SECRET_<REF> from the environment.
//
// This is the integration with every tool listed at the top of this file:
// `op run -- devbay run ...` has already put the values in devbay's
// environment, and this finds them there without devbay knowing anything about
// 1Password.
type EnvSource struct{}

func (EnvSource) Name() string        { return "env" }
func (EnvSource) Handles(string) bool { return true }

func (EnvSource) Resolve(_ context.Context, _, ref string) (string, *Grant, error) {
	v, ok := os.LookupEnv(EnvName(ref))
	if !ok {
		return "", nil, nil
	}
	return v, nil, nil
}

// EnvName is the variable a reference falls back to: "stripe/test" becomes
// DEVBAY_SECRET_STRIPE_TEST.
func EnvName(ref string) string {
	var b strings.Builder
	b.WriteString("DEVBAY_SECRET_")
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// external commands
// ---------------------------------------------------------------------------

// CommandSource shells out to a secret manager.
//
// The command is configured by the developer, not by a manifest, which is what
// makes it safe to run: a manifest cannot express a command, and this one is
// not derived from repository content. `{ref}` in the argv is replaced with
// the reference being resolved.
//
//	op:     op read op://{ref}
//	sops:   sops --extract '["{ref}"]' -d secrets.enc.yaml
//	vault:  vault kv get -field=value secret/{ref}
type CommandSource struct {
	// Label names the source in the audit log.
	Label string
	// Argv is the command, with {ref} substituted.
	Argv []string
	// Prefix limits this source to references beginning with it, so several
	// managers can coexist.
	Prefix string
	// Timeout bounds the call; a secret manager that hangs must not hang a boot.
	Timeout time.Duration
}

func (c CommandSource) Name() string {
	if c.Label != "" {
		return c.Label
	}
	if len(c.Argv) > 0 {
		return c.Argv[0]
	}
	return "command"
}

func (c CommandSource) Handles(ref string) bool {
	return c.Prefix == "" || strings.HasPrefix(ref, c.Prefix)
}

func (c CommandSource) Resolve(ctx context.Context, _, ref string) (string, *Grant, error) {
	if len(c.Argv) == 0 {
		return "", nil, errors.New("no command configured")
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := make([]string, len(c.Argv))
	for i, a := range c.Argv {
		argv[i] = strings.ReplaceAll(a, "{ref}", ref)
	}

	// Executed as an argv array with no shell, for the same reason manifests
	// are: the reference is substituted into an argument, and an argument is
	// not a command.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", nil, errors.New(msg)
	}
	// Trailing newlines are an artefact of the tool, not part of the secret,
	// and one smuggled into an API key produces a baffling 401.
	return strings.Trim(string(out), "\r\n"), nil, nil
}
