package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Clarittyai/devbay/internal/bay"
	"github.com/Clarittyai/devbay/internal/engine"
)

// register defines the tool surface.
//
// The descriptions are written for a model rather than a manual page: each one
// says when to reach for the tool and what it costs, because a model choosing
// between run_task and logs is making a decision the description has to
// inform. The alias parameter on bay_create is required on purpose -- naming
// discipline is cheapest at creation time, and an agent left to its own
// devices produces branch names nobody can read in a tab strip.
func (s *Server) register() {
	s.add(Tool{
		Name:  "bay_create",
		Title: "Create an isolated environment",
		Description: "Create a bay: an isolated, runnable instance of this repository at a branch, " +
			"with its own worktree, containers, database, ports and browser origin. " +
			"Use this before running or verifying work that should not disturb other branches. " +
			"Booting is the expensive step; pass boot=false to prepare the worktree without starting services.",
		InputSchema: object(map[string]any{
			"name": prop("string", "Short identifier for the bay. Becomes a directory and a hostname, so lowercase letters, digits and hyphens only."),
			"alias": prop("string", fmt.Sprintf(
				"Short human-readable label, max %d characters. This is what a human sees in a tab strip and in `devbay ls`, "+
					"so make it meaningful at a glance: 'oauth', not 'feature-1'.", bay.MaxAlias)),
			"branch":      prop("string", "Branch to check out. Defaults to the bay name."),
			"from_branch": prop("string", "Starting point for a branch that does not exist yet. Defaults to the current HEAD."),
			"boot":        prop("boolean", "Start the services. Defaults to true."),
		}, "name", "alias"),
		handler: s.createBay,
	})

	s.add(Tool{
		Name:        "bay_list",
		Title:       "List environments",
		Description: "List every bay with its state, alias, branch, URLs and memory use. Use this to find out what already exists before creating another.",
		InputSchema: object(nil),
		handler:     s.listBays,
	})

	s.add(Tool{
		Name:  "bay_run_task",
		Title: "Run a declared task",
		Description: "Run a task declared in devbay.yaml (for example 'unit', 'integration', 'lint') inside a bay, " +
			"and get typed results back: exit code, counts, and failures with file, line and message. " +
			"Prefer this over running a test command yourself: devbay starts only the services the task declares it needs, " +
			"so a unit suite boots nothing at all, and the failures come back structured rather than as text to parse.",
		InputSchema: object(map[string]any{
			"bay":  prop("string", "Name of the bay to run in."),
			"task": prop("string", "Task name as declared under `tasks:` in devbay.yaml."),
		}, "bay", "task"),
		handler: s.runTask,
	})

	s.add(Tool{
		Name:  "bay_logs",
		Title: "Read service logs",
		Description: "Read recent log lines from a service in a bay. Secret values are removed before the logs are returned. " +
			"Use this when a task failed for a reason the structured failures do not explain, such as a service that never started.",
		InputSchema: object(map[string]any{
			"bay":     prop("string", "Name of the bay."),
			"service": prop("string", "Service name. Defaults to the primary service."),
			"lines":   prop("integer", "How many trailing lines to return. Defaults to 100."),
		}, "bay"),
		handler: s.logs,
	})

	s.add(Tool{
		Name:  "bay_url",
		Title: "Get a service URL",
		Description: "Return the addresses of a service in a bay. `url` is what code running outside the bay should call; " +
			"`public_url` is the browser origin, which is what a browser automation tool should open. " +
			"They differ on purpose and are not interchangeable.",
		InputSchema: object(map[string]any{
			"bay":     prop("string", "Name of the bay."),
			"service": prop("string", "Service name. Defaults to the primary service."),
		}, "bay"),
		handler: s.url,
	})

	s.add(Tool{
		Name:        "bay_status",
		Title:       "Inspect one environment",
		Description: "Report a bay's state, per-service health, URLs and memory use. Use this to check whether a bay is ready before driving it.",
		InputSchema: object(map[string]any{
			"bay": prop("string", "Name of the bay."),
		}, "bay"),
		handler: s.status,
	})

	s.add(Tool{
		Name:  "bay_destroy",
		Title: "Destroy an environment",
		Description: "Remove a bay completely: containers, volumes, network, database, ports and worktree. " +
			"This deletes uncommitted work in the bay's worktree unless it was created by something else, " +
			"so commit or push first if the work matters.",
		InputSchema: object(map[string]any{
			"bay":   prop("string", "Name of the bay."),
			"force": prop("boolean", "Discard uncommitted changes in the worktree. Defaults to false, which refuses instead."),
		}, "bay"),
		handler: s.destroy,
	})
}

// ready reports whether the server has a manager behind it. A server built
// for protocol inspection alone still answers tools/list, which is what an
// agent calls before anything exists.
func (s *Server) ready() error {
	if s.mgr == nil {
		return fmt.Errorf("devbay is not attached to a repository; run this from inside a git repository with a devbay.yaml")
	}
	return nil
}

func (s *Server) add(t Tool) {
	s.tools = append(s.tools, t)
	s.index[t.Name] = &s.tools[len(s.tools)-1]
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (s *Server) createBay(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var args struct {
		Name       string `json:"name"`
		Alias      string `json:"alias"`
		Branch     string `json:"branch"`
		FromBranch string `json:"from_branch"`
		Boot       *bool  `json:"boot"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if args.Name == "" {
		return nil, errMissing("name")
	}
	if args.Alias == "" {
		return nil, errMissing("alias")
	}
	if len(args.Alias) > bay.MaxAlias {
		return nil, fmt.Errorf("alias %q is %d characters; keep it to %d or fewer so it stays readable when truncated",
			args.Alias, len(args.Alias), bay.MaxAlias)
	}

	boot := true
	if args.Boot != nil {
		boot = *args.Boot
	}
	b, err := s.mgr.Create(ctx, bay.CreateOptions{
		Name:   args.Name,
		Alias:  args.Alias,
		Branch: args.Branch,
		From:   args.FromBranch,
		Boot:   boot,
	})
	if err != nil {
		return nil, err
	}
	return s.mgr.Describe(ctx, b)
}

func (s *Server) listBays(ctx context.Context, _ json.RawMessage) (any, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	list, err := s.mgr.List(ctx)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []bay.Info{}
	}
	return map[string]any{"bays": list}, nil
}

func (s *Server) runTask(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var args struct {
		Bay  string `json:"bay"`
		Task string `json:"task"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if args.Bay == "" {
		return nil, errMissing("bay")
	}
	if args.Task == "" {
		return nil, errMissing("task")
	}

	b, ok := s.mgr.Get(args.Bay)
	if !ok {
		return nil, unknownBay(s, ctx, args.Bay)
	}
	if _, ok := b.Manifest.Tasks[args.Task]; !ok {
		// Listing what does exist turns a dead end into a next step.
		names := make([]string, 0, len(b.Manifest.Tasks))
		for n := range b.Manifest.Tasks {
			names = append(names, n)
		}
		return nil, fmt.Errorf("no task %q in devbay.yaml; declared tasks are: %s",
			args.Task, strings.Join(names, ", "))
	}
	return s.mgr.RunTask(ctx, args.Bay, args.Task)
}

func (s *Server) logs(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var args struct {
		Bay     string `json:"bay"`
		Service string `json:"service"`
		Lines   int    `json:"lines"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if args.Bay == "" {
		return nil, errMissing("bay")
	}
	b, ok := s.mgr.Get(args.Bay)
	if !ok {
		return nil, unknownBay(s, ctx, args.Bay)
	}

	service := args.Service
	if service == "" {
		service = b.Manifest.PrimaryService()
	}
	lines := args.Lines
	if lines <= 0 {
		lines = 100
	}

	// Already scrubbed by the engine; HC1 holds at the boundary rather than
	// depending on every caller remembering.
	out, err := b.Engine.Logs(ctx, service, lines)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"bay":     args.Bay,
		"service": service,
		"lines":   strings.Split(strings.TrimRight(out, "\n"), "\n"),
	}, nil
}

func (s *Server) url(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var args struct {
		Bay     string `json:"bay"`
		Service string `json:"service"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if args.Bay == "" {
		return nil, errMissing("bay")
	}
	b, ok := s.mgr.Get(args.Bay)
	if !ok {
		return nil, unknownBay(s, ctx, args.Bay)
	}

	service := args.Service
	if service == "" {
		service = b.Manifest.PrimaryService()
	}
	svc, ok := b.Manifest.Services[service]
	if !ok {
		return nil, fmt.Errorf("no service %q in this bay", service)
	}
	if svc.Port == 0 {
		return nil, fmt.Errorf("service %q exposes no port, so it has no URL", service)
	}

	res := b.Engine.Resolver()
	out := map[string]any{"bay": args.Bay, "service": service}
	if ep, err := res.Endpoint(service, engine.PlaneHost); err == nil {
		out["url"] = "http://" + ep.Addr()
	}
	out["public_url"] = res.Scheme + "://" + res.Hostname(service)
	// Spelling out which is which prevents the mistake this distinction exists
	// to prevent: using the browser origin from code, where it will not
	// resolve, or the loopback address in a browser, where it is not isolated.
	out["note"] = "call `url` from code; open `public_url` in a browser"
	return out, nil
}

func (s *Server) status(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var args struct {
		Bay string `json:"bay"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if args.Bay == "" {
		return nil, errMissing("bay")
	}
	b, ok := s.mgr.Get(args.Bay)
	if !ok {
		return nil, unknownBay(s, ctx, args.Bay)
	}
	return s.mgr.Describe(ctx, b)
}

func (s *Server) destroy(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var args struct {
		Bay   string `json:"bay"`
		Force bool   `json:"force"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if args.Bay == "" {
		return nil, errMissing("bay")
	}
	if err := s.mgr.Destroy(ctx, args.Bay, args.Force); err != nil {
		return nil, err
	}
	return map[string]any{"bay": args.Bay, "destroyed": true}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func errMissing(field string) error { return fmt.Errorf("%s is required", field) }

// unknownBay names what does exist. A bare "not found" makes an agent guess;
// the list makes the next call obvious.
func unknownBay(s *Server, ctx context.Context, name string) error {
	list, err := s.mgr.List(ctx)
	if err != nil || len(list) == 0 {
		return fmt.Errorf("no bay named %q, and none exist; create one with bay_create", name)
	}
	names := make([]string, len(list))
	for i, b := range list {
		names[i] = b.Name
	}
	return fmt.Errorf("no bay named %q; existing bays are: %s", name, strings.Join(names, ", "))
}

func object(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}
