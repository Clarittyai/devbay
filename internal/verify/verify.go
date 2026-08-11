// Package verify boots a proposed manifest and repairs it when it fails.
//
// This is what makes generated configuration trustworthy. Detection is
// imperfect and always will be: rules reach a useful majority of repositories
// and a model reaches further, but neither can be relied on to be right the
// first time. The developer does zero work not because detection is perfect,
// but because recovery from imperfect detection is automated.
//
// The loop is: propose, validate, boot, probe. A failure at any stage is fed
// back with the evidence — which service, which stage, and what it actually
// printed — and a new proposal is attempted. Bounded, because a loop that
// tries forever is a loop that burns an afternoon producing nothing; after the
// last attempt the partial manifest and the final failure are handed to the
// human, who is usually four lines from a working file.
//
// # The airlock
//
// A Patcher is untrusted. It reads repository content — READMEs, CI configs,
// dependency manifests, and now the error output of code from that repository —
// which is precisely the material an attacker can influence, and it produces
// something devbay is about to execute.
//
// So every proposal crosses one checkpoint, in this package, before anything
// runs it:
//
//   - It must parse and pass the full validator. A shell string where an argv
//     array belongs fails structurally, before any rule runs.
//   - `egress:` is stripped from every proposal, always. If content from a
//     repository could widen the network policy, an injected instruction could
//     widen the network policy, and the sandbox would be arguing with itself.
//   - A proposal that fails validation is not retried blindly: the validation
//     error is itself fed back, because a patcher that produced invalid YAML
//     can usually fix it when told what was wrong.
package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// Stage names where an attempt failed.
type Stage string

const (
	StageParse    Stage = "parse"
	StageValidate Stage = "validate"
	StageBoot     Stage = "boot"
	StagePatch    Stage = "patch"
)

// Failure describes why an attempt did not work, in the terms a patcher needs.
type Failure struct {
	Stage Stage `json:"stage"`
	// Service is the service that failed, when the failure belongs to one.
	Service string `json:"service,omitempty"`
	// Message is the error as devbay saw it.
	Message string `json:"message"`
	// Logs is the container's own output, which is usually where the real
	// cause is: a missing environment variable, a refused connection, a
	// migration that has not run.
	Logs string `json:"logs,omitempty"`
}

func (f Failure) Error() string {
	// The service is named only when the message does not already do so.
	// Engine errors are prefixed by the failing service, and repeating it
	// reads as two different problems.
	if f.Service != "" && !strings.Contains(f.Message, f.Service) {
		return string(f.Stage) + " " + f.Service + ": " + f.Message
	}
	return string(f.Stage) + ": " + f.Message
}

// Patcher proposes a revised manifest.
//
// Implementations are untrusted by construction, including the ones that call
// a model. Nothing here trusts the result: see the package comment.
type Patcher interface {
	// Patch returns revised YAML, given the current file and why it failed.
	Patch(ctx context.Context, current []byte, f Failure) ([]byte, error)
}

// PatcherFunc adapts a function to Patcher.
type PatcherFunc func(context.Context, []byte, Failure) ([]byte, error)

func (p PatcherFunc) Patch(ctx context.Context, cur []byte, f Failure) ([]byte, error) {
	return p(ctx, cur, f)
}

// Booter brings a manifest up and reports whether it works.
//
// Returning a *Failure rather than a bare error is the point: a patcher given
// "boot failed" can do nothing useful, and a patcher given the service, the
// stage and the container's own output usually can.
type Booter interface {
	Boot(ctx context.Context, m *manifest.Manifest) *Failure
}

// BooterFunc adapts a function to Booter.
type BooterFunc func(context.Context, *manifest.Manifest) *Failure

func (b BooterFunc) Boot(ctx context.Context, m *manifest.Manifest) *Failure {
	return b(ctx, m)
}

// Attempt records one pass through the loop.
type Attempt struct {
	N        int           `json:"n"`
	Manifest []byte        `json:"-"`
	Failure  *Failure      `json:"failure,omitempty"`
	Took     time.Duration `json:"took"`
}

// Result is the outcome of the loop.
type Result struct {
	// Manifest is the last proposal that was tried. On success it is the one
	// that worked; on failure it is the best available starting point, which
	// is far more useful to a human than nothing.
	Manifest []byte `json:"-"`
	// Parsed is the validated form of Manifest, when it validated.
	Parsed   *manifest.Manifest `json:"-"`
	Attempts []Attempt          `json:"attempts"`
	OK       bool               `json:"ok"`
	// Note explains why the loop stopped when that is not obvious from the
	// last failure, e.g. that no patcher was available.
	Note string `json:"note,omitempty"`
}

// LastFailure returns the failure that ended the loop, if any.
func (r *Result) LastFailure() *Failure {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if r.Attempts[i].Failure != nil {
			return r.Attempts[i].Failure
		}
	}
	return nil
}

// Loop repairs a proposed manifest until it boots.
type Loop struct {
	// Boot brings a candidate up. Required.
	Boot Booter
	// Patch proposes a revision. When nil the loop validates and boots once,
	// which is the deterministic-only path.
	Patch Patcher
	// MaxAttempts bounds the work. Zero means three.
	MaxAttempts int
	// Log receives progress.
	Log func(format string, args ...any)
}

// NoPatcher marks a result that stopped because nothing could repair it. It is
// recorded on the result rather than returned as an error: the loop ran, and
// the answer -- with the failure and the container's logs -- is the useful
// part. Returning an error here made callers discard exactly that.
const NoPatcher = "no patcher is configured, so the failure was not repaired"

// Run drives the loop over an initial proposal.
func (l *Loop) Run(ctx context.Context, initial []byte) (*Result, error) {
	if l.Boot == nil {
		return nil, errors.New("verify: a Booter is required")
	}
	logf := l.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	max := l.MaxAttempts
	if max <= 0 {
		max = 3
	}

	res := &Result{Manifest: initial}
	current := initial

	for n := 1; n <= max; n++ {
		start := time.Now()
		attempt := Attempt{N: n, Manifest: current}

		// The airlock. Every proposal crosses it, including the first, because
		// a deterministic detector is not trusted either -- it read the same
		// repository.
		m, failure := admit(current)
		if failure == nil {
			logf("  attempt %d: booting %d service(s)", n, len(m.Services))
			failure = l.Boot.Boot(ctx, m)
		} else {
			logf("  attempt %d: rejected at the %s stage", n, failure.Stage)
		}

		attempt.Took = time.Since(start)
		attempt.Failure = failure
		res.Attempts = append(res.Attempts, attempt)
		res.Manifest = current

		if failure == nil {
			res.Parsed, res.OK = m, true
			return res, nil
		}
		logf("  attempt %d failed: %s", n, failure.Error())

		if n == max {
			break
		}
		if l.Patch == nil {
			res.Note = NoPatcher
			return res, nil
		}

		next, err := l.Patch.Patch(ctx, current, *failure)
		if err != nil {
			// A patcher that errors ends the loop: retrying with the same
			// input would produce the same error, and the caller is better
			// served by the last working proposal and an honest reason.
			res.Attempts = append(res.Attempts, Attempt{
				N: n + 1,
				Failure: &Failure{
					Stage:   StagePatch,
					Message: err.Error(),
				},
			})
			return res, nil
		}
		if len(next) == 0 || string(next) == string(current) {
			// No change means the patcher has nothing left to offer, and
			// another identical attempt would waste the remaining budget.
			logf("  attempt %d: the patcher proposed no change; stopping", n)
			break
		}
		current = next
	}

	return res, nil
}

// admit is the checkpoint between an untrusted proposal and the executor.
func admit(data []byte) (*manifest.Manifest, *Failure) {
	m, err := manifest.Parse(data)
	if err != nil {
		msg := err.Error()
		// Scoped to commands. Firing on any string-into-struct mismatch meant
		// `build: ./web` was answered with a lecture about argv arrays, which
		// sends a patcher off to fix a rule that was never broken.
		if strings.Contains(msg, "manifest.Argv") {
			// The most common way a model gets this wrong, and the one worth
			// naming precisely, because "cannot unmarshal" tells it nothing.
			msg += "\n\nCommands must be argv arrays, not shell strings: write [pnpm, dev], not \"pnpm dev\". " +
				"There is no way to express a shell command in this format."
		}
		return nil, &Failure{Stage: StageParse, Message: msg}
	}

	// Stripped unconditionally, before validation, and never restored. This is
	// the single rule that keeps the airlock from being decorative: content a
	// repository can influence must not be able to widen the network policy.
	stripEgress(m)

	if r := manifest.Validate(m); !r.OK() {
		var b strings.Builder
		for _, d := range r.Errors() {
			fmt.Fprintf(&b, "%s: %s\n", d.Location(), d.Msg)
		}
		return nil, &Failure{Stage: StageValidate, Message: strings.TrimSpace(b.String())}
	}
	return m, nil
}

// stripEgress removes any network allowlist a proposal tried to grant itself.
func stripEgress(m *manifest.Manifest) {
	for _, s := range m.Services {
		if s != nil {
			s.Egress = nil
		}
	}
}

// StripEgress is the exported form, for callers admitting a manifest by hand.
func StripEgress(m *manifest.Manifest) { stripEgress(m) }
