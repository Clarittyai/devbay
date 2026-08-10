// Package scrub removes secret values from anything leaving devbay.
//
// The rule it enforces is that a secret never enters model context: not in a
// manifest, not in a prompt, not in logs returned to an agent. Everything
// upstream tries to make that true by construction -- secrets are references
// until spawn time, and the manifest rejects literals -- but an application
// will happily print its own configuration, and an error message from a
// third-party SDK will happily quote the credential it just used.
//
// So this is the last line rather than the only one. It works from the exact
// values the broker resolved, which is the one thing a pattern matcher cannot
// know, and falls back to shape-based detection for credentials that were
// never handed out by devbay at all.
package scrub

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Redaction is what replaces a secret. It names the reference rather than the
// value where one is known, so a developer reading a scrubbed log can still
// tell which credential was involved.
const Redaction = "[redacted]"

// Scrubber removes known secret values and credential-shaped strings.
//
// The zero value is usable and does shape-based scrubbing only.
type Scrubber struct {
	mu sync.RWMutex
	// values maps a literal secret to the label that replaces it.
	values map[string]string
	// ordered holds the values longest-first, so a secret that contains
	// another is replaced whole rather than leaving a fragment behind.
	ordered []string
}

// New returns an empty scrubber.
func New() *Scrubber { return &Scrubber{values: map[string]string{}} }

// Add registers a secret value and the reference it came from.
//
// Short values are ignored: a one- or two-character "secret" would match
// everywhere and redact the whole log into uselessness, which is its own kind
// of failure.
func (s *Scrubber) Add(ref, value string) {
	if len(value) < 6 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]string{}
	}
	label := "[redacted:" + ref + "]"
	if ref == "" {
		label = Redaction
	}
	s.values[value] = label

	s.ordered = s.ordered[:0]
	for v := range s.values {
		s.ordered = append(s.ordered, v)
	}
	// Longest first. Replacing a shorter secret first could leave the tail of
	// a longer one in place, which is exactly the sort of near-miss that reads
	// as safe and is not.
	sort.Slice(s.ordered, func(i, j int) bool { return len(s.ordered[i]) > len(s.ordered[j]) })
}

// Len reports how many values are registered.
func (s *Scrubber) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.values)
}

// String scrubs a single string.
func (s *Scrubber) String(in string) string {
	s.mu.RLock()
	ordered := s.ordered
	values := s.values
	s.mu.RUnlock()

	out := in
	for _, v := range ordered {
		if strings.Contains(out, v) {
			out = strings.ReplaceAll(out, v, values[v])
		}
	}
	return patterned(out)
}

// Bytes scrubs a byte slice.
func (s *Scrubber) Bytes(in []byte) []byte { return []byte(s.String(string(in))) }

// Lines scrubs each line of a log.
func (s *Scrubber) Lines(in []string) []string {
	out := make([]string, len(in))
	for i, l := range in {
		out[i] = s.String(l)
	}
	return out
}

// shapes are credentials recognisable without knowing the value.
//
// These catch what devbay never issued: a token an application fetched at
// runtime, one baked into an image, one a developer exported into their shell.
// The list is the same family the manifest validator screens for, so a
// credential that would be rejected in a manifest is also redacted in a log.
var shapes = []*regexp.Regexp{
	regexp.MustCompile(`\b(sk|rk|pk)_(live|test)_[A-Za-z0-9]{8,}`),                        // Stripe
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}`),                                    // GitHub
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),                                  // GitHub fine-grained
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}`),                                      // GitLab
	regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}`),                                       // AWS access key id
	regexp.MustCompile(`\bya29\.[A-Za-z0-9_-]{20,}`),                                      // Google OAuth
	regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{30,}`),                                        // Google API key
	regexp.MustCompile(`\bxox[baprse]-[A-Za-z0-9-]{10,}`),                                 // Slack
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`),                                     // Anthropic
	regexp.MustCompile(`\bnpm_[A-Za-z0-9]{30,}`),                                          // npm
	regexp.MustCompile(`\bdop_v1_[a-f0-9]{60,}`),                                          // DigitalOcean
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), // JWT
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// dsnPassword matches the password inside a connection string. Applications
// log their own DATABASE_URL constantly, and the host and database name are
// the useful parts of it.
var dsnPassword = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/@\s]+):([^@/\s]+)@`)

func patterned(in string) string {
	out := in
	for _, re := range shapes {
		out = re.ReplaceAllString(out, Redaction)
	}
	out = dsnPassword.ReplaceAllString(out, "$1:"+Redaction+"@")
	return out
}

// Text scrubs a string with shape detection only, for callers with no
// scrubber to hand.
func Text(in string) string { return patterned(in) }
