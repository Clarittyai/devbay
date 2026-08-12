package worktree

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Creating worktrees in parallel is the operation devbay exists to make
// parallel, and git does not make it safe on its own: concurrent
// `git worktree add` calls read each other's half-written metadata and abort
// with "failed to read .git/worktrees/<other>/commondir: Success".
//
// Probabilistic by nature, so it runs enough of them to lose reliably without
// the lock rather than once and hope.
func TestConcurrentCreatesDoNotRaceEachOther(t *testing.T) {
	repo := newRepo(t)
	m := manager(t, repo)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // line them up, so they collide rather than queue
			_, errs[i] = m.Create(CreateOptions{Name: fmt.Sprintf("par%d", i)})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("par%d: %v", i, err)
		}
	}

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list) - 1; got != n { // the main checkout is in the list too
		t.Errorf("git lists %d worktrees, want %d", got, n)
	}
}

// The lock has to exclude, or the test above is only measuring luck.
func TestTheLockActuallyExcludes(t *testing.T) {
	m := manager(t, newRepo(t))

	release, err := m.lock()
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan time.Time, 1)
	go func() {
		r2, err := m.lock()
		if err != nil {
			t.Error(err)
			close(got)
			return
		}
		got <- time.Now()
		r2()
	}()

	select {
	case <-got:
		t.Fatal("a second holder took the lock while the first still had it")
	case <-time.After(150 * time.Millisecond):
	}

	released := time.Now()
	release()

	select {
	case at, ok := <-got:
		if ok && at.Before(released) {
			t.Error("the second holder reports taking the lock before it was released")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was never handed over")
	}
}
