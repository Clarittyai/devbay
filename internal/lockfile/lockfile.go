// Package lockfile serializes work between devbay processes.
//
// devbay is a short-lived CLI, and the interesting operations are the ones
// several copies of it perform at once: two terminals, or an agent calling
// `devbay new` three times in parallel, which is the thing devbay is for. A
// mutex only orders the goroutines inside one process, so anything that reads
// shared state, modifies it, and writes it back needs a lock the operating
// system enforces across processes.
package lockfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Timeout bounds the wait. The operations this guards are sub-second, so a
// wait this long means a process died holding the lock rather than that
// someone is genuinely ahead in the queue. Failing with an explanation beats
// hanging without one.
const Timeout = 2 * time.Minute

// inProcess keeps goroutines in one process off the file lock, where each
// would otherwise block an OS thread waiting for the others.
var inProcess sync.Map // lock path -> *sync.Mutex

// Acquire takes the named lock and returns the release.
//
// Not reentrant: flock associates the lock with the open file description, so
// a second acquisition in the same process waits for the first, and a caller
// holding it would wait for itself.
func Acquire(key string) (func(), error) {
	path, err := Path(key)
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
		return nil, fmt.Errorf("lockfile: opening %s: %w", path, err)
	}

	// Polled rather than blocking, so the wait can be given up on.
	deadline := time.Now().Add(Timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			unlockProcess()
			return nil, fmt.Errorf("lockfile: locking %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			unlockProcess()
			return nil, fmt.Errorf("lockfile: another devbay process has held %s for over %s. "+
				"If none is running, delete that file", path, Timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		unlockProcess()
	}, nil
}

// Path names the lock file for a key.
//
// Hashed because a key may be an absolute path: it can contain separators, it
// can be longer than a filename may be, and two keys with the same basename
// must not collide.
func Path(key string) (string, error) {
	sum := sha256.Sum256([]byte(key))
	name := filepath.Base(key) + "-" + hex.EncodeToString(sum[:8]) + ".lock"

	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "devbay-"+name), nil
	}
	return filepath.Join(home, ".devbay", "locks", name), nil
}
