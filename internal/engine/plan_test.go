package engine

import (
	"testing"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// A boot plan must never start a service before what it needs. The fixture
// with the deepest chain is the FastAPI template: db -> wait-db -> alembic ->
// prestart -> backend -> frontend, which is exactly the ordering a flat list
// of setup commands could not express.
func TestBootPlanRespectsDependencyOrder(t *testing.T) {
	m := fixture(t, "fastapi-template")
	p, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}

	pos := map[string]int{}
	for i, s := range p.Steps {
		pos[s.Service] = i
	}
	for _, edge := range [][2]string{
		{"db", "wait-db"},
		{"wait-db", "alembic"},
		{"alembic", "prestart"},
		{"prestart", "backend"},
		{"backend", "frontend"},
	} {
		if pos[edge[0]] >= pos[edge[1]] {
			t.Errorf("%s must be planned before %s (got %d, %d)",
				edge[0], edge[1], pos[edge[0]], pos[edge[1]])
		}
	}

	if len(p.Steps) != len(m.Services) {
		t.Errorf("boot plan covers %d of %d services", len(p.Steps), len(m.Services))
	}
	for _, s := range p.Steps {
		if want := m.Services[s.Service].IsOneshot(); s.Oneshot != want {
			t.Errorf("%s: Oneshot = %v, want %v", s.Service, s.Oneshot, want)
		}
	}
}

// Waves are what make a boot fast: everything at the same depth starts at
// once, so five independent datastores cost one wave, not five.
func TestWavesGroupIndependentServices(t *testing.T) {
	m := fixture(t, "documenso")
	p, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}
	waves := p.Waves()
	if len(waves) < 2 {
		t.Fatalf("got %d waves, expected the graph to have depth", len(waves))
	}

	// db, redis, storage, mail and translate depend on nothing.
	first := map[string]bool{}
	for _, s := range waves[0] {
		first[s.Service] = true
	}
	for _, name := range []string{"db", "redis", "storage", "mail", "translate"} {
		if !first[name] {
			t.Errorf("%s depends on nothing and belongs in the first wave", name)
		}
	}
	// Everything in a wave must have all its dependencies in earlier waves.
	depth := map[string]int{}
	for _, s := range p.Steps {
		depth[s.Service] = s.Wave
	}
	for name, s := range m.Services {
		for _, dep := range s.Needs {
			if depth[dep] >= depth[name] {
				t.Errorf("%s (wave %d) depends on %s (wave %d)", name, depth[name], dep, depth[dep])
			}
		}
	}
}

// The reason tasks declare needs: a unit suite should boot nothing at all.
func TestTaskPlanMaterializesOnlyWhatIsNeeded(t *testing.T) {
	m := fixture(t, "saleor")

	for _, c := range []struct {
		task string
		want []string
	}{
		{"lint", nil},
		{"typecheck", nil},
		{"unit", []string{"db", "api", "collectstatic", "migrate", "populatedb", "redis"}},
	} {
		p, err := TaskPlan(m, c.task)
		if err != nil {
			t.Fatalf("%s: %v", c.task, err)
		}
		got := p.Services()
		if len(c.want) == 0 {
			if len(got) != 0 {
				t.Errorf("task %q boots %v; a task needing nothing must boot nothing", c.task, got)
			}
			continue
		}
		set := map[string]bool{}
		for _, s := range got {
			set[s] = true
		}
		for _, w := range c.want {
			if !set[w] {
				t.Errorf("task %q: missing %s from %v", c.task, w, got)
			}
		}
	}
}

// A task's subgraph is the transitive closure, not just the listed services:
// naming the API implies its migrations and its database.
func TestTaskPlanIsTransitive(t *testing.T) {
	m := fixture(t, "fastapi-template")
	p, err := TaskPlan(m, "unit")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range p.Services() {
		got[s] = true
	}
	// unit needs [db] and runs in backend, so backend's whole chain comes too.
	for _, want := range []string{"db", "backend", "prestart", "alembic", "wait-db", "mail"} {
		if !got[want] {
			t.Errorf("missing %s from the unit task subgraph: %v", want, p.Services())
		}
	}
	// The frontend is not involved in a backend unit test.
	if got["frontend"] {
		t.Error("unit test subgraph should not include the frontend")
	}
}

func TestTaskPlanUnknownTask(t *testing.T) {
	m := fixture(t, "gitea")
	if _, err := TaskPlan(m, "nope"); err == nil {
		t.Error("unknown task should be an error")
	}
}

// The validator rejects cycles, so a cycle reaching the engine means something
// bypassed validation. It must fail loudly rather than recurse forever.
func TestPlanRejectsCycleRatherThanHanging(t *testing.T) {
	m := &manifest.Manifest{
		Version: 1,
		Project: "acme",
		Services: map[string]*manifest.Service{
			"a": {Image: "x", Needs: []string{"b"}},
			"b": {Image: "x", Needs: []string{"a"}},
		},
	}
	done := make(chan error, 1)
	go func() {
		_, err := BootPlan(m)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cyclic graph should be an error")
		}
	case <-timeout():
		t.Fatal("BootPlan hung on a cyclic graph")
	}
}

func TestSeedPlan(t *testing.T) {
	m := fixture(t, "fastapi-template")
	p, sources, err := SeedPlan(m, "db")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) == 0 {
		t.Error("seed sources are what decide staleness; they must be reported")
	}
	// Seeding runs the whole chain that leads to prestart, in order.
	svcs := p.Services()
	if len(svcs) == 0 {
		t.Fatal("empty seed plan")
	}
	pos := map[string]int{}
	for i, s := range svcs {
		pos[s] = i
	}
	if pos["alembic"] >= pos["prestart"] {
		t.Errorf("seed plan out of order: %v", svcs)
	}

	// A service with no seed block reports nothing rather than erroring.
	if p, sources, err := SeedPlan(m, "backend"); err != nil || p != nil || sources != nil {
		t.Errorf("SeedPlan on an unseeded service = %v/%v/%v, want nils", p, sources, err)
	}
}
