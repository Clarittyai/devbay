// Package worktree manages the git worktree behind a bay.
//
// devbay bundles worktree management rather than composing with a separate
// worktree tool, because a worktree and its runtime have to be created and
// destroyed as one unit: a worktree removed while its containers still hold a
// bind mount, or containers removed while the worktree survives, are both
// leaks that show up later as confusing failures.
//
// It does not, however, insist on owning the worktree. Coding agents now
// create worktrees natively — Claude Code checks out into .claude/worktrees/
// and exposes it to the model directly — so when a worktree for the requested
// branch already exists, devbay adopts it instead of creating a second
// checkout of the same branch, which git would refuse anyway.
//
// Every git invocation here is an argv array executed without a shell, for
// the same reason the manifest requires it: branch names come from users and
// from agents, and "feat/$(whoami)" must be a branch name, not a command.
package worktree

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Worktree is one checkout.
type Worktree struct {
	// Path is the absolute path of the checkout.
	Path string
	// Branch is the branch checked out there, without the refs/heads/ prefix.
	// Empty for a detached HEAD.
	Branch string
	// Head is the commit currently checked out.
	Head string
	// Main reports whether this is the repository's primary checkout rather
	// than a linked worktree. devbay never removes the main worktree.
	Main bool
	// CreatedBranch reports that this call created the branch, rather than
	// checking out one that already existed. It matters on failure: unwinding
	// has to delete a branch devbay created, or a retry silently checks out
	// the stale one instead of the caller's corrected code.
	CreatedBranch bool
	// Adopted reports that devbay found this worktree rather than creating it —
	// typically an agent's own. Adopted worktrees are not removed on teardown,
	// because devbay did not create them and something else may still be using
	// them.
	Adopted bool
}

// Manager operates on one repository.
type Manager struct {
	// RepoRoot is the main checkout's root.
	RepoRoot string
	// Root is where devbay creates new worktrees.
	Root string

	// Log receives notes a developer needs to see. Never nil after Open.
	Log func(format string, args ...any)

	// Reclaim takes ownership of a worktree back from the containers that
	// wrote into it, and is called only when removal has already failed for
	// lack of permission.
	//
	// A container writing into the bind-mounted worktree writes as whatever
	// user it runs as, which for most images is root. On Linux that ownership
	// is the host's ownership -- there is no mapping layer -- so a build
	// artefact, a lockfile, or a test report left behind by a container is a
	// root-owned file the developer cannot delete, and `devbay rm` fails
	// having already destroyed the containers. On macOS the file sharing layer
	// maps ownership to the calling user and this never happens, which is
	// exactly why it went unnoticed until CI ran on Linux.
	//
	// A function rather than a direct call because this package deliberately
	// knows nothing about containers; the caller supplies the means.
	Reclaim func(path string) error
}

// ErrDirty is returned when removing a worktree would discard uncommitted work.
var ErrDirty = errors.New("worktree has uncommitted changes")

// Open locates the repository containing dir and returns a Manager for it.
// root is where new worktrees are created; if empty, ~/.devbay/worktrees is used.
func Open(dir, root string) (*Manager, error) {
	out, err := git(dir, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git repository: %w", dir, err)
	}
	repoRoot := strings.TrimSpace(out)

	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(home, ".devbay", "worktrees", filepath.Base(repoRoot))
	}

	// git reports fully resolved paths, so every path devbay compares against
	// one has to be resolved too. This is not a detail: on macOS /var is a
	// symlink to /private/var, and /tmp to /private/tmp, so a repository under
	// either — which includes anything in a temp directory — would fail both
	// the main-checkout check and the adoption check while looking correct.
	return &Manager{RepoRoot: resolve(repoRoot), Root: resolve(root)}, nil
}

// resolve expands symlinks in path, tolerating parts that do not exist yet by
// resolving the deepest existing ancestor and re-joining the remainder.
func resolve(path string) string {
	rest := ""
	for p := path; ; {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return path // reached the root without finding anything that exists
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// CreateOptions describes a worktree to create.
type CreateOptions struct {
	// Name is the bay name; it becomes the directory name.
	Name string
	// Branch to check out. Defaults to Name.
	Branch string
	// From is the starting point for a branch that does not exist yet.
	// Defaults to the repository's current HEAD.
	From string
}

// Create makes a worktree for opts.Branch, or adopts an existing one.
//
// Adoption is not a fallback for an error case; it is the expected path when
// an agent made the worktree first. git refuses to check out one branch into
// two worktrees, so without adoption devbay could not run a bay on a branch
// an agent was already working in — which is the common case, not a corner.
func (m *Manager) Create(opts CreateOptions) (*Worktree, error) {
	// Held across the whole operation, not just the `git worktree add`: the
	// unwind path removes a worktree and prunes, which touches the same
	// metadata a concurrent create would be reading.
	release, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer release()
	return m.create(opts)
}

func (m *Manager) create(opts CreateOptions) (*Worktree, error) {
	if opts.Name == "" {
		return nil, errors.New("worktree: name is required")
	}
	branch := opts.Branch
	if branch == "" {
		branch = opts.Name
	}

	if wt, ok, err := m.Find(branch); err != nil {
		return nil, err
	} else if ok {
		wt.Adopted = true
		return wt, nil
	}

	path := filepath.Join(m.Root, opts.Name)
	if _, err := os.Stat(path); err == nil {
		// Usually the remains of a run that was interrupted between creating
		// the directory and registering it, so the fix is a teardown rather
		// than a different name -- and saying which command does it saves the
		// developer working out that a directory under ~/.devbay is devbay's
		// to remove.
		return nil, fmt.Errorf("worktree: %s already exists but git does not know about it, "+
			"which is what an interrupted `devbay new` leaves behind. "+
			"`devbay rm %s --force` clears it, or pick another name", path, filepath.Base(path))
	}

	// Read and validate the include list before touching git. Everything that
	// can be checked without side effects is checked without side effects, so
	// the common failure — a malformed pattern — leaves nothing to unwind.
	patterns, err := m.includePatterns()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	newBranch := !m.branchExists(branch)
	if !newBranch {
		// An existing branch is checked out where it is, not recreated from
		// HEAD. Said out loud because the surprise is otherwise silent and
		// expensive: destroy a bay, commit a fix, create the bay again with
		// the same name, and it comes back on the old commit -- the fix
		// apparently ignored, with nothing on screen to explain it.
		if sha, err := git(m.RepoRoot, "rev-parse", "--short", branch); err == nil {
			m.logf("worktree: branch %s already exists; checking it out at %s (delete it or pass --branch for a fresh one)",
				branch, strings.TrimSpace(sha))
		}
	}
	args := []string{"worktree", "add"}
	if newBranch {
		from := opts.From
		if from == "" {
			from = "HEAD"
		}
		args = append(args, "-b", branch, path, from)
	} else {
		args = append(args, path, branch)
	}
	if _, err := git(m.RepoRoot, args...); err != nil {
		return nil, fmt.Errorf("worktree: creating %s: %w", path, err)
	}

	// From here on the operation has side effects, so any failure unwinds
	// them. A half-created worktree is exactly the leak this package exists to
	// prevent, and it is worse than an outright failure because it silently
	// blocks the same branch from being checked out again later.
	fail := func(err error) (*Worktree, error) {
		_, _ = git(m.RepoRoot, "worktree", "remove", "--force", path)
		_, _ = git(m.RepoRoot, "worktree", "prune")
		if newBranch {
			_, _ = git(m.RepoRoot, "branch", "-D", branch)
		}
		return nil, err
	}

	if err := copyIncluded(m.RepoRoot, path, patterns); err != nil {
		return fail(fmt.Errorf("worktree: carrying included files into %s: %w", path, err))
	}

	wt, ok, err := m.Find(branch)
	if err != nil {
		return fail(fmt.Errorf("worktree: created %s but could not list it: %w", path, err))
	}
	if !ok {
		return fail(fmt.Errorf("worktree: created %s but git does not list it", path))
	}
	wt.CreatedBranch = newBranch
	return wt, nil
}

// isPermission reports whether git failed because it could not delete a file.
//
// Matched on the message because git reports this as a plain exit status: the
// detail only exists in the text it printed ("failed to delete '<path>':
// Permission denied"), so there is no error value to compare against.
func isPermission(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "permission denied") || strings.Contains(s, "operation not permitted")
}

// DeleteBranch removes a branch, used when unwinding a failed creation.
//
// git worktree remove takes the checkout away but leaves the branch, so
// without this a failed attempt keeps its branch pointing at whatever was
// current when it ran -- and the obvious retry checks that stale commit out
// again, appearing to ignore the fix the caller just made.
func (m *Manager) DeleteBranch(branch string) error {
	if branch == "" {
		return nil
	}
	release, err := m.lock()
	if err != nil {
		return err
	}
	defer release()
	if _, err := git(m.RepoRoot, "branch", "-D", branch); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil
		}
		return fmt.Errorf("worktree: deleting branch %s: %w", branch, err)
	}
	return nil
}

// Remove deletes a worktree devbay created. It refuses to discard uncommitted
// work unless force is set, and never removes the main checkout or a worktree
// devbay adopted.
func (m *Manager) Remove(branch string, force bool) error {
	release, err := m.lock()
	if err != nil {
		return err
	}
	defer release()
	return m.remove(branch, force)
}

func (m *Manager) remove(branch string, force bool) error {
	wt, ok, err := m.Find(branch)
	if err != nil {
		return err
	}
	if !ok {
		return nil // already gone; teardown is idempotent
	}
	if wt.Main {
		return fmt.Errorf("worktree: refusing to remove the main checkout at %s", wt.Path)
	}
	if !force {
		dirty, err := m.dirty(wt.Path)
		if err != nil {
			return err
		}
		if dirty {
			return fmt.Errorf("%w: %s", ErrDirty, wt.Path)
		}
	}

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, wt.Path)
	if _, err := git(m.RepoRoot, args...); err != nil {
		// Teardown is a promise, so a failure that is recoverable is recovered
		// rather than reported. Retried once, only on a permission error, and
		// only when the caller gave us a way: silently retrying anything else
		// would turn "your worktree has uncommitted work" into a hang.
		if !isPermission(err) || m.Reclaim == nil {
			return fmt.Errorf("worktree: removing %s: %w", wt.Path, err)
		}
		if rerr := m.Reclaim(wt.Path); rerr != nil {
			return fmt.Errorf("worktree: removing %s: %w (reclaiming ownership also failed: %v)", wt.Path, err, rerr)
		}
		// `git worktree remove` is not idempotent after a partial failure: it
		// deletes what it can first, including the metadata that makes the
		// directory a worktree, so the obvious retry reports "is not a working
		// tree" and the real problem disappears behind a confusing message.
		// Finish the job directly, and let the prune below reconcile git's
		// records with what is actually on disk.
		if _, err := git(m.RepoRoot, args...); err != nil {
			if rmErr := os.RemoveAll(wt.Path); rmErr != nil {
				return fmt.Errorf("worktree: removing %s after reclaiming ownership: %w", wt.Path, rmErr)
			}
		}
	}
	// Without prune, a worktree whose directory vanished stays in git's
	// metadata and blocks re-checkout of the same branch later.
	_, _ = git(m.RepoRoot, "worktree", "prune")
	return nil
}

// List returns every worktree git knows about, main checkout included.
func (m *Manager) List() ([]*Worktree, error) {
	out, err := git(m.RepoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseList(out, m.RepoRoot), nil
}

// Find returns the worktree holding branch, if any.
func (m *Manager) Find(branch string) (*Worktree, bool, error) {
	list, err := m.List()
	if err != nil {
		return nil, false, err
	}
	for _, wt := range list {
		if wt.Branch == branch {
			// Anything outside the directory devbay creates in was made by
			// somebody else — an agent, or the user by hand.
			wt.Adopted = !strings.HasPrefix(wt.Path, m.Root+string(os.PathSeparator))
			return wt, true, nil
		}
	}
	return nil, false, nil
}

// parseList decodes `git worktree list --porcelain`.
func parseList(out, repoRoot string) []*Worktree {
	var list []*Worktree
	var cur *Worktree
	flush := func() {
		if cur != nil {
			cur.Main = cur.Path == repoRoot
			list = append(list, cur)
			cur = nil
		}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &Worktree{Path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			// stray line before any worktree header
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()
	return list
}

func (m *Manager) branchExists(branch string) bool {
	_, err := git(m.RepoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func (m *Manager) dirty(path string) (bool, error) {
	out, err := git(path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// includeFiles are the conventions devbay reads to decide which untracked
// files to carry into a new worktree — .env and friends, which are gitignored
// by design and whose absence makes a fresh checkout unrunnable.
//
// .worktreeinclude is Claude Code's file and is listed first deliberately: a
// repo that already told an agent which files to carry has already answered
// this question, and asking it again in devbay's own dialect would be rude.
var includeFiles = []string{".worktreeinclude", ".devbayinclude"}

// includePatterns reads and validates the include list, without side effects.
func (m *Manager) includePatterns() ([]string, error) {
	var patterns []string
	for _, name := range includeFiles {
		b, err := os.ReadFile(filepath.Join(m.RepoRoot, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				patterns = append(patterns, line)
			}
		}
		break // first file found wins; two sources of truth would be worse than none
	}

	for _, pat := range patterns {
		// These files are copied without review, so a pattern reaching outside
		// the repository is an exfiltration primitive rather than a
		// convenience: it would place arbitrary host files inside a directory
		// that is about to be bind-mounted into a container.
		if filepath.IsAbs(pat) || pat == ".." || strings.HasPrefix(pat, "../") ||
			strings.Contains(pat, "/../") || strings.HasSuffix(pat, "/..") {
			return nil, fmt.Errorf("include pattern %q must be a path inside the repository", pat)
		}
		if _, err := filepath.Match(pat, ""); err != nil {
			return nil, fmt.Errorf("include pattern %q: %w", pat, err)
		}
	}
	return patterns, nil
}

func copyIncluded(repoRoot, dst string, patterns []string) error {
	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(repoRoot, pat))
		if err != nil {
			return fmt.Errorf("include pattern %q: %w", pat, err)
		}
		for _, src := range matches {
			rel, err := filepath.Rel(repoRoot, src)
			if err != nil {
				return err
			}
			// Belt and braces: a glob cannot normally escape, but the result
			// is about to be written into a mounted directory.
			if strings.HasPrefix(rel, "..") {
				return fmt.Errorf("include pattern %q resolved outside the repository", pat)
			}
			if err := copyPath(src, filepath.Join(dst, rel)); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyPath(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.IsDir():
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	case fi.Mode()&os.ModeSymlink != 0:
		return nil // do not follow symlinks out of the repo
	case !fi.Mode().IsRegular():
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Preserve the source mode: these are often .env files that should not
	// become world-readable in the copy.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// git runs a git command with an argv array and no shell.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", err
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// logf reports something worth saying, if anyone is listening.
func (m *Manager) logf(format string, args ...any) {
	if m.Log != nil {
		m.Log(format, args...)
	}
}

// BranchHasWork reports whether a branch holds commits that exist nowhere else.
//
// The question teardown needs answered: a branch with no unique commits is
// bookkeeping and can go with the bay, while one carrying work must survive
// even though the bay does not. Deleting the second kind would be data loss;
// keeping the first kind is what makes `devbay rm` followed by `devbay new`
// silently resurrect an old commit.
func (m *Manager) BranchHasWork(branch string) bool {
	if branch == "" || !m.branchExists(branch) {
		return false
	}
	// Commits reachable from the branch but from no other ref. If listing
	// fails, assume there is work: keeping a branch is recoverable, deleting
	// one is not.
	out, err := git(m.RepoRoot, "rev-list", "--count", branch, "--not", "--exclude="+branch, "--branches", "--tags", "--remotes")
	if err != nil {
		return true
	}
	return strings.TrimSpace(out) != "0"
}

// Prune reconciles git's record with what is on disk.
//
// Exported because a caller that removes a worktree directory itself -- the
// remains of an interrupted create, which git never registered -- leaves git's
// administrative files behind, and the next create fails for a different
// reason than the first.
func (m *Manager) Prune() error {
	release, err := m.lock()
	if err != nil {
		return err
	}
	defer release()
	_, err = git(m.RepoRoot, "worktree", "prune")
	return err
}
