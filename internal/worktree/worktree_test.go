package worktree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo builds a real git repository in a temp dir. These tests exercise git
// itself rather than a mock, because every interesting case here — refusing to
// check a branch out twice, leaving metadata behind after a removal — is a
// behaviour of git, not of this package.
func newRepo(t *testing.T) string {
	t.Helper()
	// Resolved, because git reports resolved paths and on macOS t.TempDir()
	// hands back a path under /var, which is a symlink to /private/var. A test
	// that compares against the unresolved form would fail for a reason that
	// has nothing to do with the code under test.
	dir := resolve(t.TempDir())
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=devbay", "GIT_AUTHOR_EMAIL=devbay@example.com",
			"GIT_COMMITTER_NAME=devbay", "GIT_COMMITTER_EMAIL=devbay@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "initial")
	return dir
}

func manager(t *testing.T, repo string) *Manager {
	t.Helper()
	m, err := Open(repo, filepath.Join(t.TempDir(), "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCreateAndRemove(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	wt, err := m.Create(CreateOptions{Name: "add-oauth"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if wt.Branch != "add-oauth" {
		t.Errorf("branch = %q, want add-oauth", wt.Branch)
	}
	if wt.Adopted {
		t.Error("a worktree devbay created should not be marked adopted")
	}
	if wt.Main {
		t.Error("a linked worktree should not be marked main")
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "README.md")); err != nil {
		t.Errorf("worktree is not a real checkout: %v", err)
	}

	if err := m.Remove("add-oauth", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("worktree directory survived removal: %v", err)
	}

	// Teardown must be idempotent: devbay rm can run after a crash that
	// already removed part of the bay.
	if err := m.Remove("add-oauth", false); err != nil {
		t.Errorf("second remove should be a no-op, got %v", err)
	}

	// And git's metadata must be clean enough to reuse the branch, which is
	// what `worktree prune` is for.
	if _, err := m.Create(CreateOptions{Name: "add-oauth"}); err != nil {
		t.Errorf("recreating a removed worktree failed, so prune did not run: %v", err)
	}
}

func TestCreateFromBranch(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	wt, err := m.Create(CreateOptions{Name: "hotfix", Branch: "fix/login", From: "main"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if wt.Branch != "fix/login" {
		t.Errorf("branch = %q, want fix/login", wt.Branch)
	}
	// The bay name and the branch name are deliberately separable: agents
	// generate branch names far too long to use as a label.
	if filepath.Base(wt.Path) != "hotfix" {
		t.Errorf("directory = %q, want hotfix", filepath.Base(wt.Path))
	}
}

// Adoption is the case that matters most: an agent has already made a
// worktree for this branch, and git will not check the same branch out twice.
// Without adoption devbay simply could not run a bay on a branch an agent was
// working in.
func TestAdoptsExistingWorktree(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	// Simulate Claude Code's layout: .claude/worktrees/<name>.
	agentPath := filepath.Join(repo, ".claude", "worktrees", "refactor")
	if err := os.MkdirAll(filepath.Dir(agentPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "worktree", "add", "-b", "refactor", agentPath, "HEAD")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding agent worktree: %v\n%s", err, out)
	}

	wt, err := m.Create(CreateOptions{Name: "refactor"})
	if err != nil {
		t.Fatalf("create should adopt, not fail: %v", err)
	}
	if !wt.Adopted {
		t.Error("worktree created by something else must be marked adopted")
	}
	if wt.Path != agentPath {
		t.Errorf("adopted path = %q, want the agent's %q", wt.Path, agentPath)
	}
	// Nothing was created in devbay's own root.
	if _, err := os.Stat(filepath.Join(m.Root, "refactor")); !os.IsNotExist(err) {
		t.Error("adopting must not also create a second checkout")
	}
}

func TestRefusesToDiscardUncommittedWork(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	wt, err := m.Create(CreateOptions{Name: "wip"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "README.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = m.Remove("wip", false)
	if !errors.Is(err, ErrDirty) {
		t.Fatalf("remove of a dirty worktree = %v, want ErrDirty", err)
	}
	if _, statErr := os.Stat(wt.Path); statErr != nil {
		t.Error("a refused removal must leave the work in place")
	}
	if err := m.Remove("wip", true); err != nil {
		t.Fatalf("forced remove: %v", err)
	}
}

func TestNeverRemovesMainCheckout(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	err := m.Remove("main", false)
	if err == nil || !strings.Contains(err.Error(), "main checkout") {
		t.Fatalf("removing the main checkout = %v, want a refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "README.md")); statErr != nil {
		t.Fatal("the main checkout was damaged")
	}
}

// A fresh checkout has no gitignored .env, which makes it unrunnable. devbay
// reads Claude Code's .worktreeinclude first, because a repo that already told
// an agent which files to carry has answered this question.
func TestCarriesIncludedFiles(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	write := func(rel, body string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(".worktreeinclude", "# carried into every worktree\n.env\nconfig/local/*.json\n", 0o644)
	write(".env", "SECRET=xyz\n", 0o600)
	write("config/local/dev.json", "{}\n", 0o644)

	wt, err := m.Create(CreateOptions{Name: "feat"})
	if err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{".env", "config/local/dev.json"} {
		if _, err := os.Stat(filepath.Join(wt.Path, rel)); err != nil {
			t.Errorf("%s was not carried into the worktree: %v", rel, err)
		}
	}
	// A .env copied as world-readable would be a downgrade from the original.
	fi, err := os.Stat(filepath.Join(wt.Path, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf(".env copied with mode %v, want 0600 preserved", fi.Mode().Perm())
	}
}

func TestIncludePatternsCannotEscapeTheRepo(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	// These files are copied without review, so a pattern reaching outside the
	// repository is an exfiltration primitive, not a convenience.
	for i, pat := range []string{"../../../etc/passwd", "/etc/passwd", "config/../../secrets", ".."} {
		if err := os.WriteFile(filepath.Join(repo, ".worktreeinclude"), []byte(pat+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("esc-%d", i)
		_, err := m.Create(CreateOptions{Name: name})
		if err == nil || !strings.Contains(err.Error(), "inside the repository") {
			t.Errorf("include pattern %q = %v, want rejection", pat, err)
		}
		// Rejection must leave nothing behind: no directory, and no branch
		// squatting the name so a later legitimate create fails.
		if _, statErr := os.Stat(filepath.Join(m.Root, name)); !os.IsNotExist(statErr) {
			t.Errorf("include pattern %q: a rejected create left a worktree behind", pat)
		}
		if m.branchExists(name) {
			t.Errorf("include pattern %q: a rejected create left branch %q behind", pat, name)
		}
	}
}

// A failure after git has already made the worktree must unwind it. A
// half-created worktree is worse than an outright failure: it silently blocks
// the same branch from being checked out again.
func TestFailedCreateUnwindsCompletely(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	if err := os.WriteFile(filepath.Join(repo, ".worktreeinclude"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory where a regular file is expected makes the copy fail after
	// the worktree exists.
	if err := os.MkdirAll(filepath.Join(repo, ".env", "not-a-file"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repo, ".env"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(repo, ".env"), 0o755) })

	if _, err := m.Create(CreateOptions{Name: "unwind"}); err == nil {
		t.Fatal("create should have failed while copying")
	}
	if _, err := os.Stat(filepath.Join(m.Root, "unwind")); !os.IsNotExist(err) {
		t.Error("failed create left a worktree directory behind")
	}
	if m.branchExists("unwind") {
		t.Error("failed create left a branch behind")
	}
	// The name must be reusable.
	if err := os.Chmod(filepath.Join(repo, ".env"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(repo, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("K=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(CreateOptions{Name: "unwind"}); err != nil {
		t.Errorf("name should be reusable after a failed create: %v", err)
	}
}

func TestListIdentifiesMainCheckout(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)
	if _, err := m.Create(CreateOptions{Name: "a"}); err != nil {
		t.Fatal(err)
	}

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d worktrees, want 2", len(list))
	}
	var mains int
	for _, wt := range list {
		if wt.Main {
			mains++
			if wt.Path != repo {
				t.Errorf("main worktree path = %q, want %q", wt.Path, repo)
			}
		}
		if wt.Head == "" {
			t.Errorf("%s: no HEAD recorded", wt.Path)
		}
	}
	if mains != 1 {
		t.Errorf("got %d main checkouts, want exactly 1", mains)
	}
}

// Branch names reach this package from users and from agents. They are passed
// as argv, never through a shell, so shell metacharacters are inert.
func TestBranchNamesAreNotShellExpanded(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	canary := filepath.Join(t.TempDir(), "canary")
	// If any git call went through a shell, the substitution would run.
	branch := "feat/$(touch " + canary + ")"

	_, err := m.Create(CreateOptions{Name: "inject", Branch: branch})
	if _, statErr := os.Stat(canary); statErr == nil {
		t.Fatal("command substitution in a branch name executed; a git call is going through a shell")
	}
	if err == nil {
		// git may accept the name verbatim, which is fine — it is a string,
		// not a command. What matters is that nothing executed.
		if wt, ok, _ := m.Find(branch); ok && wt.Branch != branch {
			t.Errorf("branch stored as %q, want the literal %q", wt.Branch, branch)
		}
	}
}

func TestOpenRejectsNonRepository(t *testing.T) {
	if _, err := Open(t.TempDir(), ""); err == nil {
		t.Fatal("Open on a non-repository should fail")
	}
}

// A container that wrote into the bind-mounted worktree leaves files this
// process cannot delete, and teardown then fails having already destroyed the
// containers -- half a teardown, with a worktree nothing can remove. macOS
// maps ownership and never shows this; Linux does not, so CI found it.
//
// Simulated here by removing write permission from the directory holding a
// file, which is what makes the unlink fail, rather than by needing root.
func undeletable(t *testing.T, dir string) string {
	t.Helper()
	sub := filepath.Join(dir, "buildoutput")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "artifact"), []byte("root wrote this"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	// Restored unconditionally, or the test's own cleanup cannot remove it.
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	return sub
}

func TestRemoveReclaimsOwnershipWhenDeletionIsDenied(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	wt, err := m.Create(CreateOptions{Name: "reclaim"})
	if err != nil {
		t.Fatal(err)
	}
	sub := undeletable(t, wt.Path)

	called := 0
	m.Reclaim = func(path string) error {
		called++
		if path != wt.Path {
			t.Errorf("reclaim got %s, want the worktree %s", path, wt.Path)
		}
		return os.Chmod(sub, 0o755)
	}

	if err := m.Remove("reclaim", true); err != nil {
		t.Fatalf("removal did not recover: %v", err)
	}
	if called != 1 {
		t.Errorf("reclaim called %d times, want exactly 1", called)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Error("the worktree is still on disk")
	}
}

// Without a way to reclaim, the failure is reported rather than swallowed:
// silently leaving a worktree behind is the outcome this whole path exists to
// prevent.
func TestRemoveReportsADeniedDeletionWhenItCannotReclaim(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	wt, err := m.Create(CreateOptions{Name: "denied"})
	if err != nil {
		t.Fatal(err)
	}
	undeletable(t, wt.Path)

	err = m.Remove("denied", true)
	if err == nil {
		t.Fatal("removal reported success while the worktree is still on disk")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Errorf("the reason is not actionable: %v", err)
	}
}

// Retrying anything else would turn "you have uncommitted work" into a loop
// through a container that cannot help.
func TestReclaimIsNotUsedForOtherFailures(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	if _, err := m.Create(CreateOptions{Name: "dirty"}); err != nil {
		t.Fatal(err)
	}
	m.Reclaim = func(string) error {
		t.Error("reclaim was called for a failure that has nothing to do with permissions")
		return nil
	}
	wt, _, _ := m.Find("dirty")
	if err := os.WriteFile(filepath.Join(wt.Path, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("dirty", false); !errors.Is(err, ErrDirty) {
		t.Errorf("err = %v, want ErrDirty", err)
	}
}
