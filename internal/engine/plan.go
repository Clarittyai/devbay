package engine

import (
	"fmt"
	"sort"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// Step is one unit of a boot plan.
type Step struct {
	Service string
	// Oneshot steps run to completion and the plan waits for exit 0.
	Oneshot bool
	// Wave is the dependency depth. Everything in a wave can start
	// concurrently; wave N starts once wave N-1 is healthy.
	Wave int
}

// Plan is an ordered set of steps that brings some part of a bay up.
type Plan struct {
	Steps []Step
}

// Services returns the service names in the plan, in order.
func (p *Plan) Services() []string {
	out := make([]string, len(p.Steps))
	for i, s := range p.Steps {
		out[i] = s.Service
	}
	return out
}

// Waves groups the steps by dependency depth.
func (p *Plan) Waves() [][]Step {
	if len(p.Steps) == 0 {
		return nil
	}
	max := 0
	for _, s := range p.Steps {
		if s.Wave > max {
			max = s.Wave
		}
	}
	waves := make([][]Step, max+1)
	for _, s := range p.Steps {
		waves[s.Wave] = append(waves[s.Wave], s)
	}
	return waves
}

// BootPlan returns the plan that brings the whole bay up.
func BootPlan(m *manifest.Manifest) (*Plan, error) {
	all := make([]string, 0, len(m.Services))
	for name := range m.Services {
		all = append(all, name)
	}
	return planFor(m, all)
}

// TaskPlan returns the plan that materializes only what a task needs.
//
// This is the reason tasks declare `needs`. Because an agent calls a named
// task rather than a shell command, devbay knows in advance which services the
// run touches and can start those and nothing else -- so a unit suite with
// `needs: []` boots zero containers and returns in the time the test takes,
// rather than the time a full stack takes to come up.
func TaskPlan(m *manifest.Manifest, task string) (*Plan, error) {
	t, ok := m.Tasks[task]
	if !ok {
		return nil, fmt.Errorf("unknown task %q", task)
	}
	roots := append([]string{}, t.Needs...)
	// The task has to run somewhere. If it names a container explicitly, that
	// service is part of the subgraph even when it is not in `needs`.
	if t.In != "" {
		roots = append(roots, t.In)
	}
	return planFor(m, roots)
}

// planFor computes the transitive closure of roots and orders it by depth.
func planFor(m *manifest.Manifest, roots []string) (*Plan, error) {
	depth := map[string]int{}
	// A cycle would recurse forever. The manifest validator rejects cycles, so
	// reaching one here means something bypassed validation -- fail loudly
	// rather than hang.
	visiting := map[string]bool{}

	var resolve func(string, []string) (int, error)
	resolve = func(name string, path []string) (int, error) {
		if d, done := depth[name]; done {
			return d, nil
		}
		if visiting[name] {
			return 0, fmt.Errorf("dependency cycle reached the engine: %v", append(path, name))
		}
		s, ok := m.Services[name]
		if !ok {
			return 0, fmt.Errorf("unknown service %q", name)
		}
		visiting[name] = true

		d := 0
		for _, dep := range s.Needs {
			dd, err := resolve(dep, append(path, name))
			if err != nil {
				return 0, err
			}
			if dd+1 > d {
				d = dd + 1
			}
		}
		visiting[name] = false
		depth[name] = d
		return d, nil
	}

	for _, root := range roots {
		if _, err := resolve(root, nil); err != nil {
			return nil, err
		}
	}

	steps := make([]Step, 0, len(depth))
	for name, d := range depth {
		steps = append(steps, Step{
			Service: name,
			Oneshot: m.Services[name].IsOneshot(),
			Wave:    d,
		})
	}
	// Sorted by wave, then by name, so a plan is deterministic and therefore
	// diffable between runs.
	sort.Slice(steps, func(i, j int) bool {
		if steps[i].Wave != steps[j].Wave {
			return steps[i].Wave < steps[j].Wave
		}
		return steps[i].Service < steps[j].Service
	})
	return &Plan{Steps: steps}, nil
}

// SeedPlan returns the oneshots whose completion defines a service's seeded
// state, in dependency order, along with the globs that decide staleness.
func SeedPlan(m *manifest.Manifest, service string) (*Plan, []string, error) {
	s, ok := m.Services[service]
	if !ok {
		return nil, nil, fmt.Errorf("unknown service %q", service)
	}
	if s.Seed == nil {
		return nil, nil, nil
	}
	p, err := planFor(m, s.Seed.After)
	if err != nil {
		return nil, nil, err
	}
	return p, s.Seed.Sources, nil
}
