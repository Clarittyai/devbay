package bay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Clarittyai/devbay/internal/engine"
	"github.com/Clarittyai/devbay/internal/manifest"
	"github.com/Clarittyai/devbay/internal/verify"
	"github.com/Clarittyai/devbay/internal/worktree"
)

// Booter boots a candidate manifest in a throwaway bay and tears it down.
//
// The bay is disposable on purpose. Verification exists to answer one question
// -- does this configuration actually work -- and answering it by leaving
// containers, volumes and a port block behind would make `devbay init` a
// command that quietly costs resources every time it is run.
type Booter struct {
	m        *Manager
	worktree string
	name     string
}

// NewBooter returns a Booter that verifies against a worktree.
func (m *Manager) NewBooter(worktree, name string) *Booter {
	return &Booter{m: m, worktree: worktree, name: name}
}

// Boot brings a candidate up and reports the first thing that went wrong.
//
// The failure carries the service and its own logs, because a patcher handed
// "boot failed" can do nothing useful with it, and the cause is nearly always
// in what the container printed: a missing variable, a refused connection, a
// migration that has not run.
func (b *Booter) Boot(ctx context.Context, m *manifest.Manifest) *verify.Failure {
	// The candidate was written by a detector reading the repository, or by a
	// model reading the repository, and a README is repository content too.
	// Whatever the airlock boots executes here with this developer's checkout
	// and this developer's machine, so R2 applies with more force during
	// verification than during an ordinary boot, not less.
	if f := b.m.approvalGate(ctx, m); f != nil {
		return f
	}

	eng, err := engine.New(ctx, engine.Options{
		Manifest: m,
		Bay:      b.name,
		Worktree: b.worktree,
		Ports:    b.m.alloc,
		Scrubber: b.m.scrub,
		Secrets:  b.m.secretsFor(b.name),
		Egress:   b.m.egress,
		Log:      b.m.Log,
	})
	if err != nil {
		return &verify.Failure{Stage: verify.StageBoot, Message: err.Error()}
	}
	// Torn down however this returns, including on a panic further up: a
	// verification run that leaks is worse than one that fails.
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		_ = eng.Down(c)
		_ = eng.Close()
	}()

	plan, err := engine.BootPlan(m)
	if err != nil {
		return &verify.Failure{Stage: verify.StageBoot, Message: err.Error()}
	}
	if err := eng.Up(ctx, plan); err != nil {
		f := &verify.Failure{Stage: verify.StageBoot, Message: err.Error()}
		// Name the service and attach its output, which is the difference
		// between a repairable failure and an opaque one.
		if svc := serviceFromError(err.Error(), m); svc != "" {
			f.Service = svc
			if logs, lerr := eng.Logs(context.WithoutCancel(ctx), svc, 60); lerr == nil {
				f.Logs = logs
			}
		}
		return f
	}
	return nil
}

// serviceFromError finds which service an engine error refers to.
func serviceFromError(msg string, m *manifest.Manifest) string {
	for name := range m.Services {
		if containsWord(msg, name) {
			return name
		}
	}
	return ""
}

func containsWord(s, word string) bool {
	for i := 0; i+len(word) <= len(s); i++ {
		if s[i:i+len(word)] != word {
			continue
		}
		before := i == 0 || !isWordByte(s[i-1])
		after := i+len(word) == len(s) || !isWordByte(s[i+len(word)])
		if before && after {
			return true
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c == '-' || c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// VerifyManifest boots a candidate against a scratch worktree of the current
// branch, repairing it when a patcher is configured.
func (m *Manager) VerifyManifest(ctx context.Context, candidate []byte, patcher verify.Patcher) (*verify.Result, error) {
	// A scratch checkout, so verification never disturbs the working copy and
	// a failed run leaves nothing behind.
	wt, err := m.wt.Create(worktree.CreateOptions{Name: "devbay-verify", Branch: "devbay-verify"})
	if err != nil {
		return nil, fmt.Errorf("bay: preparing a scratch worktree: %w", err)
	}
	defer func() {
		_ = m.wt.Remove("devbay-verify", true)
		if wt.CreatedBranch {
			_ = m.wt.DeleteBranch("devbay-verify")
		}
	}()

	// The candidate is written into the scratch checkout so the services it
	// describes see the file they were generated from.
	if err := os.WriteFile(filepath.Join(wt.Path, "devbay.yaml"), candidate, 0o644); err != nil {
		return nil, err
	}

	loop := &verify.Loop{
		Boot:  m.NewBooter(wt.Path, "devbay-verify"),
		Patch: patcher,
		Log:   m.Log,
	}
	return loop.Run(ctx, candidate)
}
