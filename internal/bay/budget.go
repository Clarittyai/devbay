package bay

import (
	"context"
	"os"
	"sort"
	"strconv"
)

// DefaultMaxResident is how many bays devbay will leave running at once.
//
// Five, because that is the number the whole design was sized against: five
// bays of a real application, on one developer's machine, without the machine
// becoming unusable. It is a default rather than a limit -- a developer who
// wants eight can say so -- and it is enforced by cooling, never by refusing.
const DefaultMaxResident = 5

// maxResident reads the budget.
func maxResident() int {
	if v := os.Getenv("DEVBAY_MAX_BAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		if v == "0" || v == "off" {
			return 0 // no budget; the developer is managing memory themselves
		}
	}
	return DefaultMaxResident
}

// makeRoom cools the oldest resident bay when a new one would exceed the
// budget.
//
// devbay has no daemon, so there is no process watching memory pressure and
// nothing can evict a bay while nobody is looking. What there is instead is
// this: every command is a short-lived process, and the moment a new bay is
// created is exactly the moment the budget is about to be exceeded. Doing the
// scheduling there costs nothing and needs nobody resident.
//
// Cooling rather than freezing, because freezing does not free memory --
// `docker pause` stops scheduling, not allocation, measured at 30.7 MiB
// running and 30.0 MiB frozen. Only stopping the containers returns anything,
// and a cold bay keeps its volumes, its worktree and its ports, so bringing it
// back is `devbay thaw`.
//
// HC7: the focused bay is never evicted. It is the one the developer is
// looking at, and a scheduler that stops it has done the single most
// disruptive thing available to it in exchange for a few hundred megabytes.
func (m *Manager) makeRoom(ctx context.Context, incoming string) {
	budget := maxResident()
	if budget <= 0 {
		return
	}

	type resident struct {
		name    string
		created int64
		focused bool
	}
	var running []resident

	recs, err := m.store.List(ctx, m.projectFor())
	if err != nil {
		return
	}
	for _, r := range recs {
		if r.Name == incoming {
			continue
		}
		b, ok := m.Get(r.Name)
		if !ok {
			continue
		}
		st, err := b.Engine.State(ctx)
		if err != nil || st.ReclaimsMemory() {
			continue // already cold: it is costing nothing
		}
		// The focused bay counts against the budget even though it can never
		// be the one cooled. It is using the memory either way, and leaving it
		// out of the count is how a budget of five quietly becomes six.
		running = append(running, resident{name: r.Name, created: r.CreatedAt, focused: r.Focused})
	}

	// The incoming bay counts against the budget, which is the point: five
	// means five afterwards, not five before.
	over := len(running) + 1 - budget
	if over <= 0 {
		return
	}

	// Oldest first. Recency of use would be the better signal and devbay does
	// not have it -- a bay is used by talking to it in a browser, which no
	// command observes -- so age is the honest approximation, and the message
	// says which bay went so the developer can disagree.
	sort.Slice(running, func(i, j int) bool { return running[i].created < running[j].created })
	candidates := make([]resident, 0, len(running))
	for _, r := range running {
		if !r.focused {
			candidates = append(candidates, r)
		}
	}
	for i := 0; i < over && i < len(candidates); i++ {
		name := candidates[i].name
		b, ok := m.Get(name)
		if !ok {
			continue
		}
		m.Log("bay: %d bays are already running, so cooling %q to make room "+
			"(its data and branch are kept; `devbay thaw %s` brings it back)",
			len(running), name, name)
		if err := b.Engine.Cool(ctx); err != nil {
			m.Log("bay: could not cool %q: %v", name, err)
		}
	}
}

// projectFor is the project name these bays belong to.
func (m *Manager) projectFor() string {
	name, err := m.projectName()
	if err != nil {
		return ""
	}
	return name
}
