package broker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one line in the credential log.
//
// There is no value field, and there must never be one. The log exists so a
// developer can answer "what was this environment given, and when" months
// later; a log that answers it by storing the credentials would be a worse
// leak than the one it was meant to detect.
type Event struct {
	At        time.Time `json:"at"`
	Action    string    `json:"action"`
	Bay       string    `json:"bay,omitempty"`
	Ref       string    `json:"ref,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Minted    bool      `json:"minted,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// Audit is an append-only record of credential grants and revocations.
//
// Append-only in the way that matters here: it is opened for append, each
// event is one line, and nothing rewrites it. That is not tamper-proofing --
// anything with write access can still truncate the file -- but it does mean a
// crash cannot lose earlier entries and a concurrent writer cannot interleave
// half a line into another.
type Audit struct {
	mu   sync.Mutex
	path string
}

// OpenAudit opens or creates the log. An empty path uses ~/.devbay/audit.jsonl.
func OpenAudit(path string) (*Audit, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".devbay", "audit.jsonl")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	// Created 0600: it records which credentials went where, which is not
	// secret but is reconnaissance.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &Audit{path: path}, nil
}

// Record appends an event.
func (a *Audit) Record(e Event) error {
	if a == nil {
		return nil
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// One write, one line: a partial line from an interrupted write would
	// corrupt the entry after it as well as its own.
	_, err = f.Write(append(line, '\n'))
	return err
}

// Events reads the log back, oldest first.
func (a *Audit) Events() ([]Event, error) {
	if a == nil {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	body, err := os.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []Event
	for _, line := range splitLines(body) {
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			// A truncated final line is expected after a crash and must not
			// make the whole log unreadable.
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Path reports where the log lives, for `devbay doctor`.
func (a *Audit) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// String renders an event for a human.
func (e Event) String() string {
	s := fmt.Sprintf("%s %-14s %s", e.At.Format(time.RFC3339), e.Action, e.Ref)
	if e.Bay != "" {
		s += " bay=" + e.Bay
	}
	if e.Provider != "" {
		s += " via=" + e.Provider
	}
	if !e.ExpiresAt.IsZero() {
		s += " expires=" + e.ExpiresAt.Format(time.RFC3339)
	}
	return s
}
