package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Serializing the operations git does not serialize itself.
//
// `git worktree add` is not safe to run concurrently against one repository.
// Creating a worktree writes several files under .git/worktrees/<name>/, and
// creating another one at the same time walks that directory to validate what
// is already there. Catch it mid-write and git aborts the whole command:
//
//	fatal: failed to read .git/worktrees/par4/commondir: Success
//
// which is CI failing to create five bays in parallel, on the operation devbay
// exists to make parallel. It is intermittent, it names the wrong worktree, and
// the errno it prints is "Success", so it reads as anything except a race.
//
// The lock is a file lock rather than a mutex because the racing parties are
// usually separate processes: two terminals, or an agent calling `devbay new`
// twice at once. A mutex would only order the goroutines inside one of them.
//
// It is keyed by the repository's common git directory, so every worktree of a
// repository shares one lock and unrelated repositories never wait on each
// other, and it lives under ~/.devbay rather than inside .git, because a lock
// file is devbay's bookkeeping and does not belong in the user's repository.

// lockTimeout bounds the wait. Creating a worktree is a sub-second operation,
// so a wait this long means a crashed process left the file locked rather than
// that someone is genuinely ahead in the queue. Failing with an explanation
// beats hanging with none.
const lockTimeout = 2 * time.Minute

// inProcess keeps goroutines in one process off the file lock, where each would
// otherwise block an OS thread waiting for the others.
var inProcess sync.Map // lock path -> *sync.Mutex

// lock takes the repository's worktree lock and returns the release.
//
// Not reentrant: flock associates the lock with the open file description, so a
// second acquisition in the same process waits for the first, and a caller
// holding it would wait for itself. Only the exported mutating entry points
// take it, and everything they call reaches git directly.
func (m *Manager) lock() (func(), error) {
	path, err := m.lockPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	mu, _ := inProcess.LoadOrStore(path, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	unlockProcess := mu.(*sync.Mutex).Unlock

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		unlockProcess()
		return nil, fmt.Errorf("worktree: opening lock %s: %w", path, err)
	}

	// Polled rather than blocking, so the wait can be given up on.
	deadline := time.Now().Add(lockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			unlockProcess()
			return nil, fmt.Errorf("worktree: locking %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			unlockProcess()
			return nil, fmt.Errorf("worktree: another devbay process has been working on %s for over %s. "+
				"If none is running, delete %s", m.RepoRoot, lockTimeout, path)
		}
		time.Sleep(20 * time.Millisecond)
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		unlockProcess()
	}, nil
}

// lockPath names the lock for this repository.
//
// Hashed because the key is an absolute path: it can contain separators, it can
// be longer than a filename may be, and two repositories with the same basename
// must not share a lock.
func (m *Manager) lockPath() (string, error) {
	key := m.commonDir()
	sum := sha256.Sum256([]byte(key))
	name := filepath.Base(m.RepoRoot) + "-" + hex.EncodeToString(sum[:8]) + ".lock"

	home, err := os.UserHomeDir()
	if err != nil {
		// Without a home directory, next to the repository's git data. Still
		// shared by every process working on this repository, which is the
		// only property that matters.
		return filepath.Join(key, "devbay-worktree.lock"), nil
	}
	return filepath.Join(home, ".devbay", "locks", name), nil
}

// commonDir is the .git directory shared by every worktree of the repository,
// which is what makes the lock cover all of them. It falls back to the repo
// root, which is still stable and still shared.
func (m *Manager) commonDir() string {
	out, err := git(m.RepoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return m.RepoRoot
	}
	if d := resolve(strings.TrimSpace(out)); d != "" {
		return d
	}
	return m.RepoRoot
}
