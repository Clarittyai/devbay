package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Clarittyai/devbay/internal/bay"
)

func tempFile(t *testing.T, name string) string { return filepath.Join(t.TempDir(), name) }
func tempDir(t *testing.T, name string) string  { return filepath.Join(t.TempDir(), name) }

// With enforcement on, a bay's services reach only what the manifest declares.
//
// This runs through the whole stack rather than against the egress package
// alone, because the interesting failure is not "iptables did not work" -- that
// has its own tests -- but "the engine never applied the policy", which is
// invisible from inside the package that would have applied it.
func TestEgressIsEnforcedThroughTheWholeStack(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	repo := newRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	m, err := bay.Open(ctx, bay.Options{
		Dir:          repo,
		StatePath:    tempFile(t, "state.db"),
		WorktreeRoot: tempDir(t, "worktrees"),
		NoProxy:      true,
		Egress:       true,
		Log:          func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err != nil {
		t.Skipf("cannot open manager: %v", err)
	}
	defer m.Close()
	m.SetSecret("e2e/token", canary)

	b, err := m.Create(ctx, bay.CreateOptions{Name: "locked", Alias: "locked", Boot: true})
	if err != nil {
		t.Fatalf("creating the bay: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_ = m.Destroy(c, "locked", true)
	})

	// The fixture declares no egress, so nothing in it should reach outside.
	res, err := b.Engine.RunTask(ctx, "reach-out")
	if err != nil {
		t.Fatalf("running the reachability task: %v", err)
	}
	if res.Succeeded() {
		t.Errorf("a service with no declared egress reached the internet:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "blocked") {
		t.Logf("task output: %s", res.Output)
	}

	// The bay itself must still work, or the policy has broken the product.
	unit, err := b.Engine.RunTask(ctx, "unit")
	if err != nil {
		t.Fatalf("running unit: %v", err)
	}
	if !unit.Succeeded() {
		t.Errorf("enforcement broke an ordinary task: %s", unit.Output)
	}
}
