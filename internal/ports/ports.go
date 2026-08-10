// Package ports allocates the host ports a bay publishes on.
//
// Two properties matter, and only one of them is negotiable.
//
// Determinism is desirable: the same branch should land on the same ports
// across daemon restarts, because humans bookmark URLs and agents cache them.
// A hash of the branch name gives that for free.
//
// Correctness is not negotiable, and a hash alone cannot provide it. Hashing
// into N buckets collides at the rate the birthday problem predicts -- with 90
// buckets and five bays that is better than a one-in-ten chance, and a
// collision is total, because both bays would publish every service on
// identical ports. Nor does a hash know which ports something else on the
// machine has already taken.
//
// So the hash is the first guess, not the answer. The allocation it suggests
// is confirmed against persisted state and against the host, probed forward
// when either objects, and then recorded. The common case stays deterministic;
// the uncommon case stays correct.
package ports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// The range devbay allocates from. Deliberately high and contiguous: the
// alternative of offsetting each service from its own base port lets two
// services in different bays land on the same number whenever their base ports
// differ by less than the offset range.
const (
	RangeStart = 40000
	RangeEnd   = 49000
	BlockSize  = 10
	blocks     = (RangeEnd - RangeStart) / BlockSize
)

// ErrExhausted is returned when every block in the range is taken.
var ErrExhausted = errors.New("ports: no free block in the allocation range")

// Block is a contiguous run of host ports reserved for one bay.
type Block struct {
	Project string
	Bay     string
	Base    int
	Size    int
}

// Ports returns every port in the block.
func (b Block) Ports() []int {
	out := make([]int, b.Size)
	for i := range out {
		out[i] = b.Base + i
	}
	return out
}

// Allocator hands out and remembers blocks.
type Allocator struct {
	db *sql.DB

	// free reports whether a port can be bound. Replaced in tests.
	free func(int) bool
}

// Open opens or creates the allocation store. An empty path uses
// ~/.devbay/state.db.
func Open(path string) (*Allocator, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".devbay", "state.db")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS port_blocks (
			project    TEXT    NOT NULL,
			bay        TEXT    NOT NULL,
			base       INTEGER NOT NULL,
			size       INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (project, bay)
		);
		-- The uniqueness of a base is enforced by the database rather than by
		-- the allocation loop, so two daemons racing for the same block cannot
		-- both win.
		CREATE UNIQUE INDEX IF NOT EXISTS port_blocks_base ON port_blocks(base);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ports: preparing store: %w", err)
	}
	return &Allocator{db: db, free: portIsFree}, nil
}

// Close releases the store.
func (a *Allocator) Close() error { return a.db.Close() }

// Get returns an existing allocation.
func (a *Allocator) Get(ctx context.Context, project, bay string) (Block, bool, error) {
	var b Block
	err := a.db.QueryRowContext(ctx,
		`SELECT project, bay, base, size FROM port_blocks WHERE project = ? AND bay = ?`,
		project, bay).Scan(&b.Project, &b.Bay, &b.Base, &b.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return Block{}, false, nil
	}
	if err != nil {
		return Block{}, false, err
	}
	return b, true, nil
}

// Allocate reserves a block large enough for n ports.
//
// It is idempotent: calling it again for the same bay returns the same block,
// which is what makes a URL survive a daemon restart. An existing block that
// is too small is replaced, because a manifest that grew a service should not
// silently reuse a block it has outgrown.
func (a *Allocator) Allocate(ctx context.Context, project, bay string, n int) (Block, error) {
	if project == "" || bay == "" {
		return Block{}, errors.New("ports: project and bay are required")
	}
	size := roundUp(n)
	if size > RangeEnd-RangeStart {
		return Block{}, fmt.Errorf("ports: %d ports is more than the whole range holds", n)
	}

	if existing, ok, err := a.Get(ctx, project, bay); err != nil {
		return Block{}, err
	} else if ok {
		if existing.Size >= size {
			return existing, nil
		}
		if err := a.Release(ctx, project, bay); err != nil {
			return Block{}, err
		}
	}

	// The hash is the starting point, and probing walks forward from it, so a
	// bay whose preferred block is taken still lands somewhere predictable
	// rather than somewhere arbitrary.
	start := (PreferredBase(project, bay) - RangeStart) / BlockSize

	for i := 0; i < blocks; i++ {
		base := RangeStart + ((start+i)%blocks)*BlockSize
		if base+size > RangeEnd {
			continue // would run past the end of the range
		}
		if !a.hostFree(base, size) {
			continue
		}
		b := Block{Project: project, Bay: bay, Base: base, Size: size}
		if err := a.insert(ctx, b); err != nil {
			if isTaken(err) {
				continue // another daemon won the race; try the next block
			}
			return Block{}, err
		}
		return b, nil
	}
	return Block{}, ErrExhausted
}

// PreferredBase is the block a bay gets when nothing is in the way.
//
// Exported because it is part of the contract, not an implementation detail: a
// bay's URL is predictable precisely because this function is. Tests use it to
// arrange the collisions that prove probing works.
func PreferredBase(project, bay string) int {
	return RangeStart + int(crc32.ChecksumIEEE([]byte(project+"/"+bay))%uint32(blocks))*BlockSize
}

// Release frees a bay's block. Teardown must call this, or the range leaks a
// block per bay ever created.
func (a *Allocator) Release(ctx context.Context, project, bay string) error {
	_, err := a.db.ExecContext(ctx,
		`DELETE FROM port_blocks WHERE project = ? AND bay = ?`, project, bay)
	return err
}

// Assign maps a sorted list of port keys onto a bay's block.
//
// The keys are sorted by the caller so the mapping depends only on which ports
// a manifest declares, not on map iteration order -- otherwise the same bay
// would publish a service on a different port each boot, and determinism would
// be a claim rather than a property.
func (b Block) Assign(keys []string) map[string]int {
	out := make(map[string]int, len(keys))
	for i, k := range keys {
		if i >= b.Size {
			break
		}
		out[k] = b.Base + i
	}
	return out
}

func (a *Allocator) insert(ctx context.Context, b Block) error {
	_, err := a.db.ExecContext(ctx,
		`INSERT INTO port_blocks (project, bay, base, size, created_at) VALUES (?, ?, ?, ?, ?)`,
		b.Project, b.Bay, b.Base, b.Size, time.Now().Unix())
	return err
}

// hostFree reports whether every port in a candidate block can be bound.
//
// The store knows what devbay allocated; it cannot know what anything else on
// the machine is using. Without this check the first symptom of a conflict
// would be a container that fails to start for reasons the manifest cannot
// explain.
func (a *Allocator) hostFree(base, size int) bool {
	for p := base; p < base+size; p++ {
		if !a.free(p) {
			return false
		}
	}
	return true
}

func portIsFree(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func roundUp(n int) int {
	if n <= BlockSize {
		return BlockSize
	}
	return ((n + BlockSize - 1) / BlockSize) * BlockSize
}

// isTaken reports the unique-index violation that means another allocation
// claimed this block first.
func isTaken(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}
