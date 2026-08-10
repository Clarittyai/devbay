package bay

import (
	"os"
	"strings"
	"sync"

	"github.com/Clarittyai/devbay/internal/scrub"
)

// Secrets resolves ${secret:path} references at container spawn time.
//
// This is deliberately the smallest thing that can be correct rather than an
// attempt at a secrets manager. The ecosystem already converged on one shape --
// `op run --`, `direnv exec`, `sops exec-env`, `dotenvx run --` all inject
// values into a subprocess and hold nothing -- so the useful thing to build is
// a consumer of those, not a competitor to them. What lives here is the
// registry that resolution and scrubbing share, plus an environment fallback.
//
// Two properties matter more than where the values come from:
//
//   - A value registered here is registered with the scrubber in the same
//     call. Resolving a secret without teaching the scrubber about it would
//     hand an application a credential that devbay could no longer recognise
//     in that application's own logs.
//   - Lookup happens when a container is created, not when a manifest is read,
//     so a value is never held longer than it has to be.
type Secrets struct {
	mu     sync.RWMutex
	values map[string]string
	scrub  *scrub.Scrubber
}

// NewSecrets returns a registry that reports every value it hands out to s.
func NewSecrets(s *scrub.Scrubber) *Secrets {
	return &Secrets{values: map[string]string{}, scrub: s}
}

// Set registers a value for a reference.
func (s *Secrets) Set(ref, value string) {
	s.mu.Lock()
	s.values[ref] = value
	s.mu.Unlock()
	if s.scrub != nil {
		s.scrub.Add(ref, value)
	}
}

// Lookup resolves a reference, falling back to the environment.
//
// The environment fallback is what makes devbay work with every tool listed
// above without integrating with any of them: `op run -- devbay run ...`
// already puts the values in devbay's environment, and this finds them there.
func (s *Secrets) Lookup(ref string) (string, bool) {
	s.mu.RLock()
	v, ok := s.values[ref]
	s.mu.RUnlock()
	if ok {
		return v, true
	}

	if v, ok := os.LookupEnv(EnvName(ref)); ok {
		// Registered on first use so the scrubber knows about a value that
		// arrived through the environment rather than through Set.
		s.Set(ref, v)
		return v, true
	}
	return "", false
}

// EnvName is the environment variable a reference falls back to:
// "stripe/test" becomes DEVBAY_SECRET_STRIPE_TEST.
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

// Known lists the registered references, never the values.
func (s *Secrets) Known() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.values))
	for k := range s.values {
		out = append(out, k)
	}
	return out
}
