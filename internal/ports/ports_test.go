package ports

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
)

func newAllocator(t *testing.T) *Allocator {
	t.Helper()
	a, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	// Assume the host is empty unless a test says otherwise, so results do not
	// depend on what happens to be listening on the developer's machine.
	a.free = func(int) bool { return true }
	return a
}

// The property that makes URLs worth bookmarking: the same branch lands on the
// same ports across daemon restarts.
func TestAllocationIsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a.free = func(int) bool { return true }
	first, err := a.Allocate(context.Background(), "acme", "add-oauth", 4)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()

	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.free = func(int) bool { return true }
	second, err := b.Allocate(context.Background(), "acme", "add-oauth", 4)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("allocation changed across restart:\n  %+v\n  %+v", first, second)
	}
}

func TestAllocateIsIdempotent(t *testing.T) {
	a := newAllocator(t)
	ctx := context.Background()

	first, err := a.Allocate(ctx, "acme", "b1", 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := a.Allocate(ctx, "acme", "b1", 3)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("call %d returned a different block: %+v vs %+v", i, again, first)
		}
	}
}

// The bug this package exists to fix. Hashing into buckets collides; the
// allocator must resolve that rather than hand two bays the same ports.
func TestManyBaysNeverShareAPort(t *testing.T) {
	a := newAllocator(t)
	ctx := context.Background()

	seen := map[int]string{}
	for i := 0; i < 400; i++ {
		bay := fmt.Sprintf("feature-%d", i)
		b, err := a.Allocate(ctx, "acme", bay, 10)
		if err != nil {
			t.Fatalf("%s: %v", bay, err)
		}
		for _, p := range b.Ports() {
			if other, dup := seen[p]; dup {
				t.Fatalf("port %d handed to both %s and %s", p, other, bay)
			}
			seen[p] = bay
		}
	}
	if len(seen) != 400*BlockSize {
		t.Errorf("allocated %d ports across 400 bays, want %d", len(seen), 400*BlockSize)
	}
}

// Two bay names that hash to the same bucket must both work. Under the
// original scheme this was a total collision.
func TestHashCollisionStillYieldsDistinctBlocks(t *testing.T) {
	a := newAllocator(t)
	ctx := context.Background()

	// Find two names that genuinely collide rather than assuming a pair does.
	first, err := a.Allocate(ctx, "acme", "alpha", 4)
	if err != nil {
		t.Fatal(err)
	}
	var colliding string
	for i := 0; i < 20000 && colliding == ""; i++ {
		name := fmt.Sprintf("probe-%d", i)
		if PreferredBase("acme", name) == first.Base {
			colliding = name
		}
	}
	if colliding == "" {
		t.Skip("no colliding name found in the probe range")
	}

	second, err := a.Allocate(ctx, "acme", colliding, 4)
	if err != nil {
		t.Fatalf("a colliding bay must still allocate: %v", err)
	}
	if second.Base == first.Base {
		t.Fatalf("%q and %q both got base %d", "alpha", colliding, first.Base)
	}
	t.Logf("%q wanted %d, got %d", colliding, first.Base, second.Base)
}

// The store knows what devbay allocated; it cannot know what else is on the
// machine. A block whose ports are already bound must be skipped, or the first
// symptom is a container that will not start.
func TestSkipsPortsBoundBySomethingElse(t *testing.T) {
	a := newAllocator(t)
	ctx := context.Background()

	want := PreferredBase("acme", "blocked")
	a.free = func(p int) bool { return p < want || p >= want+BlockSize }

	b, err := a.Allocate(ctx, "acme", "blocked", 4)
	if err != nil {
		t.Fatal(err)
	}
	if b.Base == want {
		t.Fatalf("allocated %d even though the host has it bound", want)
	}
}

// The same check against a real listener, so the host probe itself is tested
// rather than only its stub.
func TestRealListenerIsDetected(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind a loopback port here")
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	if portIsFree(port) {
		t.Errorf("port %d is bound but reported free", port)
	}
	l.Close()
	if !portIsFree(port) {
		t.Errorf("port %d was released but is still reported busy", port)
	}
}

func TestReleaseFreesTheBlock(t *testing.T) {
	a := newAllocator(t)
	ctx := context.Background()

	first, err := a.Allocate(ctx, "acme", "temp", 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Release(ctx, "acme", "temp"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := a.Get(ctx, "acme", "temp"); err != nil || ok {
		t.Fatalf("block survived release (ok=%v, err=%v)", ok, err)
	}
	// Released means reusable, otherwise the range leaks a block per bay ever
	// created.
	again, err := a.Allocate(ctx, "acme", "temp", 4)
	if err != nil {
		t.Fatal(err)
	}
	if again.Base != first.Base {
		t.Errorf("reallocated to %d, want the freed %d", again.Base, first.Base)
	}
}

// A manifest that grows past its block must get a bigger one rather than
// silently reusing a block it has outgrown.
func TestGrowingBeyondTheBlockReallocates(t *testing.T) {
	a := newAllocator(t)
	ctx := context.Background()

	small, err := a.Allocate(ctx, "acme", "grow", 4)
	if err != nil {
		t.Fatal(err)
	}
	big, err := a.Allocate(ctx, "acme", "grow", 14)
	if err != nil {
		t.Fatal(err)
	}
	if big.Size <= small.Size {
		t.Errorf("size did not grow: %d -> %d", small.Size, big.Size)
	}
	if big.Size < 14 {
		t.Errorf("block of %d cannot hold 14 ports", big.Size)
	}
}

// Port assignment must depend only on which ports the manifest declares, or
// the same bay publishes a service on a different port each boot.
func TestAssignIsStable(t *testing.T) {
	b := Block{Base: 40000, Size: 10}
	keys := []string{"api", "db", "mail", "mail/smtp", "web"}

	first := b.Assign(keys)
	for i := 0; i < 50; i++ {
		if got := b.Assign(keys); !equal(got, first) {
			t.Fatalf("assignment changed between calls:\n  %v\n  %v", first, got)
		}
	}
	// Distinct keys get distinct ports, all inside the block.
	seen := map[int]bool{}
	for k, p := range first {
		if p < b.Base || p >= b.Base+b.Size {
			t.Errorf("%s got %d, outside the block", k, p)
		}
		if seen[p] {
			t.Errorf("%s duplicates port %d", k, p)
		}
		seen[p] = true
	}
}

func TestAllocationsStayInRange(t *testing.T) {
	a := newAllocator(t)
	ctx := context.Background()

	for i := 0; i < 200; i++ {
		b, err := a.Allocate(ctx, "acme", fmt.Sprintf("bay-%d", i), 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range b.Ports() {
			if p < RangeStart || p >= RangeEnd {
				t.Fatalf("port %d is outside [%d,%d)", p, RangeStart, RangeEnd)
			}
		}
	}
}

func TestExhaustionIsReportedNotWrappedAround(t *testing.T) {
	a := newAllocator(t)
	ctx := context.Background()
	a.free = func(int) bool { return false } // nothing on the host is available

	_, err := a.Allocate(ctx, "acme", "nowhere", 4)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
}

func TestRejectsEmptyIdentifiers(t *testing.T) {
	a := newAllocator(t)
	ctx := context.Background()
	if _, err := a.Allocate(ctx, "", "b", 1); err == nil {
		t.Error("empty project should be rejected")
	}
	if _, err := a.Allocate(ctx, "p", "", 1); err == nil {
		t.Error("empty bay should be rejected")
	}
}

func equal(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
