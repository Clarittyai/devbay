// Package bay is the orchestration layer: it turns "make me a bay" into a
// worktree, a port block, a set of containers, and a hostname, and turns
// "remove it" back into nothing.
//
// The pieces below it each do one thing and are independently testable. This
// package exists because creating and destroying them has to be atomic: a
// worktree without containers is a checkout nobody asked for, containers
// without a worktree are pointing at a directory that no longer exists, and a
// port block without either is a leak that is invisible until allocation
// starts failing.
package bay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/approve"
	"github.com/Clarittyai/devbay/internal/broker"
	"github.com/Clarittyai/devbay/internal/egress"
	"github.com/Clarittyai/devbay/internal/engine"
	"github.com/Clarittyai/devbay/internal/manifest"
	"github.com/Clarittyai/devbay/internal/ports"
	"github.com/Clarittyai/devbay/internal/proxy"
	"github.com/Clarittyai/devbay/internal/scrub"
	"github.com/Clarittyai/devbay/internal/worktree"
)

// Bay is one running instance of a repository.
type Bay struct {
	Name   string
	Branch string
	// Alias is the short human label. Agents generate branch names far too
	// long to read in a tab strip, so a bay carries a separate name capped
	// short enough to survive being truncated.
	Alias string

	Worktree string
	Adopted  bool

	Manifest *manifest.Manifest
	Engine   *engine.Engine
}

// Info is the serialisable view of a bay, and is what crosses the MCP boundary.
type Info struct {
	Name     string            `json:"name"`
	Alias    string            `json:"alias"`
	Branch   string            `json:"branch"`
	State    string            `json:"state"`
	Worktree string            `json:"worktree"`
	Adopted  bool              `json:"adopted,omitempty"`
	URLs     map[string]string `json:"urls,omitempty"`
	Ports    map[string]int    `json:"ports,omitempty"`
	Services []ServiceInfo     `json:"services,omitempty"`
	MemoryMB int64             `json:"memory_mb,omitempty"`
	Focused  bool              `json:"focused,omitempty"`
}

// ServiceInfo is one service's observable state.
type ServiceInfo struct {
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}

// Manager owns every bay for one repository.
type Manager struct {
	RepoRoot string

	cli     *client.Client
	wt      *worktree.Manager
	alloc   *ports.Allocator
	prox    *proxy.Proxy
	scrub   *scrub.Scrubber
	secrets *Secrets
	broker  *broker.Broker
	egress  *egress.Enforcer
	store   *store
	appr    *approve.Store

	mu   sync.Mutex
	bays map[string]*Bay

	Log func(format string, args ...any)
}

// Options configure a Manager.
type Options struct {
	// Dir is any path inside the repository.
	Dir string
	// StatePath is the SQLite file; empty uses ~/.devbay/state.db.
	StatePath string
	// WorktreeRoot is where new worktrees go; empty uses ~/.devbay/worktrees.
	WorktreeRoot string
	// ProxyPort is the host port for bay hostnames; 0 tries 80 then 8080.
	ProxyPort int
	// AdminPort is where the proxy's config API is published, on loopback.
	AdminPort int
	// NoProxy disables hostname routing, leaving bays reachable at
	// 127.0.0.1:<port> only.
	NoProxy bool
	// Egress enforces per-service network allowlists. Off by default because
	// it costs a privileged sidecar per service; on, a service reaches only
	// what its manifest declares.
	Egress bool
	// AuditPath is the credential log; empty uses ~/.devbay/audit.jsonl.
	AuditPath string
	// SecretCommand is a secret manager to shell out to, with {ref}
	// substituted -- for example ["op", "read", "op://{ref}"]. Configured by
	// the developer, never by a manifest.
	SecretCommand []string
	Log           func(format string, args ...any)
}

// Open prepares a manager for the repository containing opts.Dir.
func Open(ctx context.Context, opts Options) (*Manager, error) {
	logf := opts.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	wt, err := worktree.Open(opts.Dir, opts.WorktreeRoot)
	if err != nil {
		return nil, err
	}
	wt.Log = logf
	alloc, err := ports.Open(opts.StatePath)
	if err != nil {
		return nil, err
	}
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		alloc.Close()
		return nil, fmt.Errorf("bay: connecting to Docker: %w", err)
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		alloc.Close()
		return nil, fmt.Errorf("bay: Docker is not responding; is the daemon running? %w", err)
	}

	sc := scrub.New()

	st, err := openStore(opts.StatePath)
	if err != nil {
		alloc.Close()
		return nil, err
	}

	appr, err := approve.Open(opts.StatePath)
	if err != nil {
		alloc.Close()
		st.Close()
		return nil, err
	}

	m := &Manager{
		RepoRoot: wt.RepoRoot,
		cli:      cli,
		wt:       wt,
		alloc:    alloc,
		scrub:    sc,
		secrets:  NewSecrets(sc),
		store:    st,
		appr:     appr,
		bays:     map[string]*Bay{},
		Log:      logf,
	}

	// Teardown must not be defeated by a container having written root-owned
	// files into the worktree, which on Linux is what most images do. The
	// worktree package knows nothing about containers, so it is handed the
	// means rather than the mechanism.
	wt.Reclaim = func(path string) error {
		return m.reclaimOwnership(ctx, path)
	}

	// The broker resolves ${secret:...} and owns the lifetime of anything it
	// mints. Sources are consulted most-specific first.
	audit, err := broker.OpenAudit(opts.AuditPath)
	if err != nil {
		logf("bay: credential auditing is unavailable: %v", err)
	}
	m.broker = broker.New(audit, sc, logf)
	if gh, err := broker.GitHubAppFromEnv(); err != nil {
		logf("bay: GitHub App minting is not configured: %v", err)
	} else if gh != nil {
		m.broker.Add(gh)
		logf("bay: GitHub tokens will be minted per bay and revoked on teardown")
	}
	if len(opts.SecretCommand) > 0 {
		m.broker.Add(broker.CommandSource{Argv: opts.SecretCommand})
	}
	// The environment is last, so a minting or managed source always wins over
	// a value someone exported by hand.
	m.broker.Add(broker.EnvSource{})

	if opts.Egress {
		m.egress = egress.New(cli, logf)
	}

	if !opts.NoProxy {
		m.prox = proxy.New(cli, logf)
		if err := m.prox.Ensure(ctx, opts.ProxyPort, opts.AdminPort); err != nil {
			// A missing proxy costs hostnames, not function: bays still work
			// at 127.0.0.1:<port>, which is what agents and probes use anyway.
			// Failing the whole manager here would trade a degraded feature
			// for no tool at all.
			logf("bay: continuing without hostname routing: %v", err)
			m.prox = nil
		}
	}

	if err := m.rehydrate(ctx); err != nil {
		logf("bay: could not restore every known bay: %v", err)
	}
	return m, nil
}

// rehydrate reconstructs bays recorded by a previous process.
//
// A bay is not a process-lifetime object: its containers, port block and
// worktree outlive whatever created them. A record whose worktree has been
// deleted by hand is dropped rather than resurrected, because the containers
// behind it are already pointing at nothing.
func (m *Manager) rehydrate(ctx context.Context) error {
	project, err := m.projectName()
	if err != nil || project == "" {
		return nil // no manifest at the repo root; nothing to restore against
	}
	records, err := m.store.List(ctx, project)
	if err != nil {
		return err
	}

	var errs []error
	for _, r := range records {
		if _, err := os.Stat(r.Worktree); err != nil {
			m.Log("bay: dropping %q; its worktree %s is gone", r.Name, r.Worktree)
			_ = m.store.Delete(ctx, r.Name)
			continue
		}
		mf, err := loadManifest(r.Worktree)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
			continue
		}
		eng, err := engine.New(ctx, engine.Options{
			Manifest: mf, Bay: r.Name, Worktree: r.Worktree,
			Ports: m.alloc, Proxy: m.prox, Scrubber: m.scrub,
			Secrets: m.secretsFor(r.Name), Egress: m.egress, Log: m.Log,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
			continue
		}
		if r.Focused {
			_ = eng.Focus(ctx, true)
		}
		m.bays[r.Name] = &Bay{
			Name: r.Name, Branch: r.Branch, Alias: r.Alias,
			Worktree: r.Worktree, Adopted: r.Adopted,
			Manifest: mf, Engine: eng,
		}
	}

	m.republishOrphanedRoutes(ctx)
	return errors.Join(errs...)
}

// republishOrphanedRoutes restores hostnames for bays the proxy has forgotten.
//
// Normally a no-op: the proxy carries its own table across invocations. It
// matters whenever a bay is running and the proxy does not know how to reach
// it -- the proxy container being newer than the bays after a restart or an
// upgrade, or a route lost to some earlier mishap. The bay is up and reachable
// on its port, but the hostname a developer bookmarked answers 404 and nothing
// tells them why.
//
// Checked per bay rather than only when the whole table is empty. A single
// forgotten bay is the more likely shape of this, and it is exactly the one a
// whole-table check misses: one bay answering while its neighbour 404s.
func (m *Manager) republishOrphanedRoutes(ctx context.Context) {
	if m.prox == nil || len(m.bays) == 0 {
		return
	}
	routed := map[string]bool{}
	for _, r := range m.prox.Routes() {
		routed[r.Bay] = true
	}

	var restored []string
	for _, name := range sortedBayNames(m.bays) {
		b := m.bays[name]
		if b.Engine == nil || routed[name] {
			continue
		}
		// A cold bay has nothing listening, so a route would point at nothing.
		// Cooling withdraws routes on purpose, and putting them back here would
		// undo that.
		if st, err := b.Engine.State(ctx); err != nil || st == engine.StateCold {
			continue
		}
		if err := b.Engine.Republish(ctx); err != nil {
			m.Log("bay: could not restore routes for %s: %v", name, err)
			continue
		}
		restored = append(restored, name)
	}
	if len(restored) > 0 {
		m.Log("bay: restored hostnames for %s", strings.Join(restored, ", "))
	}
}

// sortedBayNames keeps republishing deterministic.
func sortedBayNames(bays map[string]*Bay) []string {
	out := make([]string, 0, len(bays))
	for name := range bays {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// warnManifestDrift reports when the working copy's manifest differs from the
// one the bay will actually run.
func (m *Manager) warnManifestDrift(worktree string) {
	find := func(dir string) ([]byte, string, bool) {
		for _, name := range ManifestNames {
			p := filepath.Join(dir, name)
			if b, err := os.ReadFile(p); err == nil {
				return b, p, true
			}
		}
		return nil, "", false
	}
	working, path, ok := find(m.RepoRoot)
	if !ok {
		return
	}
	inBay, _, ok := find(worktree)
	if !ok || bytes.Equal(working, inBay) {
		return
	}
	m.Log("note: %s has uncommitted changes; this bay runs the committed version. Commit them to take effect.", path)
}

// projectName reads the project from the manifest at the repository root.
func (m *Manager) projectName() (string, error) {
	mf, err := loadManifest(m.RepoRoot)
	if err != nil {
		return "", err
	}
	return mf.Project, nil
}

// Close releases the manager's resources. Bays keep running.
func (m *Manager) Close() error {
	var errs []error
	if m.store != nil {
		errs = append(errs, m.store.Close())
	}
	if m.appr != nil {
		errs = append(errs, m.appr.Close())
	}
	if m.alloc != nil {
		errs = append(errs, m.alloc.Close())
	}
	if m.cli != nil {
		errs = append(errs, m.cli.Close())
	}
	return errors.Join(errs...)
}

// CreateOptions describe a bay to create.
type CreateOptions struct {
	Name   string
	Branch string
	From   string
	Alias  string
	// Boot brings the services up. When false the worktree is prepared and
	// the manifest validated, but nothing is started.
	Boot bool
}

// MaxAlias is the longest useful label. Past this a browser tab truncates it
// and the label stops doing its job.
const MaxAlias = 12

// Create makes a bay, and removes every trace of it if any step fails.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (*Bay, error) {
	if opts.Name == "" {
		return nil, errors.New("bay: name is required")
	}
	m.mu.Lock()
	if _, exists := m.bays[opts.Name]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("bay: %q already exists", opts.Name)
	}
	m.mu.Unlock()

	branch := opts.Branch
	if branch == "" {
		branch = opts.Name
	}
	alias := opts.Alias
	if alias == "" {
		alias = DeriveAlias(branch)
	}

	wt, err := m.wt.Create(worktree.CreateOptions{Name: opts.Name, Branch: branch, From: opts.From})
	if err != nil {
		return nil, err
	}

	// Anything that fails after this point unwinds, because a half-made bay
	// is worse than none: it holds a branch, a port block, and possibly
	// containers, all invisibly.
	unwind := func(cause error) (*Bay, error) {
		c := context.WithoutCancel(ctx)
		if !wt.Adopted {
			_ = m.wt.Remove(branch, true)
			// The branch goes too, when this call created it. Leaving it
			// behind makes the obvious retry check out the stale commit and
			// fail again for the same reason the caller just fixed.
			if wt.CreatedBranch {
				_ = m.wt.DeleteBranch(branch)
			}
		}
		_ = m.alloc.Release(c, "", opts.Name)
		return nil, cause
	}

	// Set when the bay came up with something broken in it. The bay still
	// exists, so this travels back beside it rather than instead of it.
	var degraded error

	mf, err := loadManifestFor(wt.Path, m.RepoRoot)
	if err != nil {
		return unwind(err)
	}
	res := manifest.Validate(mf)
	if !res.OK() {
		return unwind(fmt.Errorf("bay: %s is not valid: %w", mf.Path, res.Err()))
	}
	if err := m.RequireApprovals(ctx, mf, res); err != nil {
		return unwind(err)
	}

	// A bay is a fresh checkout, so it sees the manifest as committed -- not
	// whatever is currently in the working copy. That is the right behaviour
	// for reproducibility, and a genuine trap while iterating: editing
	// devbay.yaml and creating a bay would otherwise appear to do nothing.
	m.warnManifestDrift(wt.Path)

	// Report files are written into the worktree, so without this a single
	// test run leaves the checkout dirty -- which shows up as noise in `git
	// status` and, worse, makes `devbay rm` refuse to remove a bay whose only
	// modification is an artifact devbay itself generated.
	if err := excludeGenerated(wt.Path, mf); err != nil {
		m.Log("bay: could not exclude generated reports: %v", err)
	}

	eng, err := engine.New(ctx, engine.Options{
		Manifest: mf,
		Bay:      opts.Name,
		Worktree: wt.Path,
		Ports:    m.alloc,
		Proxy:    m.prox,
		Scrubber: m.scrub,
		Secrets:  m.secretsFor(opts.Name),
		Egress:   m.egress,
		Log:      m.Log,
	})
	if err != nil {
		return unwind(err)
	}

	b := &Bay{
		Name:     opts.Name,
		Branch:   branch,
		Alias:    alias,
		Worktree: wt.Path,
		Adopted:  wt.Adopted,
		Manifest: mf,
		Engine:   eng,
	}

	if opts.Boot {
		// Before this one starts, not after: the point is to stay inside the
		// budget rather than to exceed it and then recover.
		m.makeRoom(ctx, opts.Name)

		plan, err := engine.BootPlan(mf)
		if err != nil {
			return unwind(err)
		}
		bootErr := eng.Up(ctx, plan)

		// Booting runs containers over the worktree -- installs, builds,
		// migrations -- and they run as root. The developer's next act is to
		// edit the code, so the tree has to be theirs before `devbay new`
		// returns rather than the first time they try to save a file.
		m.EnsureWritable(ctx, b.Worktree)

		if bootErr != nil && !eng.HasContainers(ctx) {
			// Nothing was created -- an image that could not be pulled, a
			// context that could not be built -- so there is nothing to look
			// at, and a bay kept for inspection would be an empty shell that
			// holds the name and the branch against a retry.
			return unwind(bootErr)
		}
		if bootErr != nil {
			// The bay is kept. devbay is an orchestration layer, and Docker
			// leaves a stack's containers in place when one service fails to
			// come up -- the rest keep running and the broken one can be
			// inspected. Tearing the whole bay down instead destroyed the only
			// evidence of what went wrong: the developer got an error, no
			// containers, and no way to read the log that explained it. It also
			// made devbay unable to run stacks Docker runs, which for an
			// orchestration layer is the wrong way round.
			//
			// So the bay is registered, its healthy services keep serving, and
			// the failure is reported with the one command that shows the rest.
			degraded = bootErr
		}
	}

	if err := m.store.Save(ctx, record{
		Name: b.Name, Alias: b.Alias, Branch: b.Branch,
		Worktree: b.Worktree, Project: mf.Project, Adopted: b.Adopted,
	}); err != nil {
		if opts.Boot {
			_ = eng.Down(context.WithoutCancel(ctx))
		}
		return unwind(fmt.Errorf("bay: recording %q: %w", opts.Name, err))
	}

	m.mu.Lock()
	m.bays[opts.Name] = b
	m.mu.Unlock()

	if degraded != nil {
		return b, &BootError{Bay: opts.Name, Err: degraded}
	}
	return b, nil
}

// Get returns a bay by name.
func (m *Manager) Get(name string) (*Bay, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bays[name]
	return b, ok
}

// List returns every bay, sorted by name.
func (m *Manager) List(ctx context.Context) ([]Info, error) {
	m.mu.Lock()
	names := make([]string, 0, len(m.bays))
	for n := range m.bays {
		names = append(names, n)
	}
	m.mu.Unlock()
	sort.Strings(names)

	out := make([]Info, 0, len(names))
	for _, n := range names {
		b, ok := m.Get(n)
		if !ok {
			continue
		}
		info, err := m.Describe(ctx, b)
		if err != nil {
			// One unreachable bay must not hide the rest, which is exactly
			// when a supervisor most needs the list.
			info = Info{Name: b.Name, Alias: b.Alias, Branch: b.Branch, State: "unknown"}
		}
		out = append(out, info)
	}
	return out, nil
}

// Describe reports a bay's current state.
func (m *Manager) Describe(ctx context.Context, b *Bay) (Info, error) {
	state, err := b.Engine.State(ctx)
	if err != nil {
		return Info{}, err
	}
	info := Info{
		Name:     b.Name,
		Alias:    b.Alias,
		Branch:   b.Branch,
		State:    string(state),
		Worktree: b.Worktree,
		Adopted:  b.Adopted,
		URLs:     b.Engine.URLs(),
		Focused:  b.Engine.Focused(),
		Ports:    map[string]int{},
	}

	res := b.Engine.Resolver()
	for _, key := range engine.PortKeys(b.Manifest) {
		svc, _, named := strings.Cut(key, "/")
		if named {
			continue
		}
		if ep, err := res.Endpoint(svc, engine.PlaneHost); err == nil {
			info.Ports[svc] = ep.Port
		}
	}

	statuses, err := b.Engine.Status(ctx)
	if err == nil {
		for _, s := range statuses {
			si := ServiceInfo{Name: s.Service, State: s.State}
			if ep, err := res.Endpoint(s.Service, engine.PlaneHost); err == nil {
				si.URL = "http://" + ep.Addr()
			}
			info.Services = append(info.Services, si)
		}
	}
	if mem, err := b.Engine.Memory(ctx); err == nil {
		info.MemoryMB = int64(mem / (1 << 20))
	}
	return info, nil
}

// Destroy removes a bay completely.
//
// The order matters: containers, volumes and network first, then the port
// block, then the worktree. Removing the worktree while containers still
// bind-mount it would leave them pointing at a path that no longer exists.
func (m *Manager) Destroy(ctx context.Context, name string, force bool) error {
	b, ok := m.Get(name)
	if !ok {
		// A bay devbay has no record of may still have left something on
		// disk: `devbay new` creates the worktree before anything else, so an
		// interrupted one leaves a directory that git does not know about and
		// that the next `new` with the same name refuses to write over. With
		// nothing here to clear it, that name is unusable for good -- and the
		// obvious command to reach for is the one that just said the bay does
		// not exist.
		if force && m.clearStrandedWorktree(name) {
			return nil
		}
		return m.notFound(ctx, name)
	}

	var errs []error

	// Credentials go first. A token revoked after the containers are gone is
	// still revoked, but a teardown that fails half way would otherwise leave
	// a live credential behind with nothing left to point at it.
	if m.broker != nil {
		if err := m.broker.Revoke(ctx, name); err != nil {
			errs = append(errs, err)
		}
	}

	if err := b.Engine.Down(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := b.Engine.Close(); err != nil {
		errs = append(errs, err)
	}

	// A worktree another bay is still using is not this bay's to remove.
	//
	// Two bays on one branch share one checkout: git allows a branch to be
	// checked out once, so the second bay adopts the first one's worktree
	// rather than making a copy, which is how two agents work on the same
	// branch with a stack each. Adoption protects the second bay's teardown
	// but not the first's -- and the first one owns the directory. Destroying
	// it took the files out from under a bay that was still running, including
	// whatever had not been committed yet.
	if shared := m.otherBayUsing(ctx, b); shared != "" {
		m.Log("bay: worktree kept: bay %q is still using it", shared)
	} else if !b.Adopted || m.wtIsOurs(b.Worktree) {
		// An adopted worktree belongs to something else -- usually an agent
		// that created it first -- so devbay leaves it alone. Removing it
		// would delete work devbay never created.
		//
		// Unless devbay created it: the second bay on a branch adopts the
		// first bay's checkout, and that one is devbay's own. Leaving it
		// because it happens to be adopted would mean the last bay out never
		// cleans up, which is a directory and a branch left behind for every
		// pair of bays that ever shared a branch.
		// Checked before the worktree goes, while the branch is still
		// meaningful.
		hasWork := m.wt.BranchHasWork(b.Branch)
		if err := m.wt.Remove(b.Branch, force); err != nil {
			errs = append(errs, err)
		} else if !hasWork {
			// A branch with no commits of its own is bookkeeping, and keeping
			// it makes `devbay rm` followed by `devbay new` come back on the
			// old commit -- the fix you just committed apparently ignored.
			// A branch that does carry work is kept, and said so.
			if err := m.wt.DeleteBranch(b.Branch); err != nil {
				m.Log("bay: could not delete branch %s: %v", b.Branch, err)
			}
		} else {
			m.Log("bay: branch %s has commits, so it was kept; `git branch -D %s` to discard them",
				b.Branch, b.Branch)
		}
		// The per-project directory that held it goes too, once empty. Left
		// behind, these accumulate one per repository ever used and make
		// ~/.devbay look like it is still holding something.
		if dir := filepath.Dir(b.Worktree); dir != "" && dir != string(filepath.Separator) {
			_ = os.Remove(dir) // fails harmlessly while other bays remain
		}
	}

	// The record goes last. If an earlier step failed, the bay is still known
	// and can be destroyed again; forgetting it first would strand whatever
	// survived.
	if err := m.store.Delete(ctx, name); err != nil {
		errs = append(errs, err)
	}

	m.mu.Lock()
	delete(m.bays, name)
	m.mu.Unlock()
	return errors.Join(errs...)
}

// clearStrandedWorktree removes the remains of an interrupted create.
//
// Only what devbay itself would have made: a directory under its own worktree
// root, for this project, with no bay record pointing at it. Anything the
// developer put there lives somewhere else.
func (m *Manager) clearStrandedWorktree(name string) bool {
	root := m.wt.Root
	if root == "" {
		return false
	}
	// Root already ends in this repository's directory, which is what Create
	// joins the bay name onto.
	path := filepath.Join(root, name)
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		return false
	}
	m.Log("bay: %q is not a bay, but %s is left over from an interrupted create; removing it", name, path)
	if err := os.RemoveAll(path); err != nil {
		m.Log("bay: could not remove %s: %v", path, err)
		return false
	}
	// git keeps its own record of a worktree, and a stale one makes the next
	// create fail for a different reason than the first.
	_ = m.wt.Prune()
	return true
}

// notFound explains a name this repository has no bay for.
//
// Bays are scoped to a project, and the state database is not: a name missing
// here may exist perfectly well one directory away. "does not exist" is then
// false, and it is false in the least helpful direction -- an agent that has
// changed directory is told to create a bay that is already running, and a
// developer goes looking for a bug in devbay's bookkeeping. Saying which
// project holds it turns a dead end into an instruction.
func (m *Manager) notFound(ctx context.Context, name string) error {
	return m.NotFound(ctx, name)
}

// otherBayUsing names a live bay, other than this one, whose worktree is the
// same directory. Empty when this bay has the checkout to itself.
func (m *Manager) otherBayUsing(ctx context.Context, b *Bay) string {
	if b.Worktree == "" {
		return ""
	}
	all, err := m.store.worktrees(ctx)
	if err != nil {
		// Without the list there is no evidence either way, and the safe
		// direction is the one that does not delete a directory: teardown
		// continues, and the worktree stays.
		return "shared (could not confirm)"
	}
	for name, path := range all {
		if name != b.Name && sameDir(path, b.Worktree) {
			return name
		}
	}
	return ""
}

// wtIsOurs reports whether a worktree is one devbay created, which is the
// difference between a checkout it may remove and an agent's own that it may
// not. Decided by location, the same way adoption is decided.
func (m *Manager) wtIsOurs(path string) bool {
	if m.wt == nil || m.wt.Root == "" || path == "" {
		return false
	}
	root, err := filepath.Abs(m.wt.Root)
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	// Inside the root, and not the root itself or a path that climbs out of it.
	return rel != "." && !strings.HasPrefix(rel, "..")
}

// sameDir compares two paths as directories, tolerating the ways the same one
// gets spelled: a trailing separator, a relative form, an unresolved symlink.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	clean := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		return filepath.Clean(p)
	}
	return clean(a) == clean(b)
}

// OwningProject names the project a bay belongs to, when that is some project
// other than this repository's. Bays are scoped to a project and the state
// database is not, so a name absent here may be running one directory away.
func (m *Manager) OwningProject(ctx context.Context, name string) (string, bool) {
	r, ok, err := m.store.Get(ctx, name)
	if err != nil || !ok {
		return "", false
	}
	if mf, mferr := loadManifest(m.RepoRoot); mferr == nil && mf.Project == r.Project {
		return "", false
	}
	return r.Project, true
}

// NotFound explains a name this repository has no bay for.
func (m *Manager) NotFound(ctx context.Context, name string) error {
	r, ok, err := m.store.Get(ctx, name)
	if err != nil || !ok {
		return fmt.Errorf("bay: %q does not exist", name)
	}
	// The manifest is read rather than cached because this path runs when
	// something is already wrong, and a stale project name would make the
	// message confidently misleading. It may also be missing entirely -- a
	// directory with no devbay.yaml is a very normal place to mistype a
	// command from -- and that is still worth answering: which project owns
	// the bay is the useful half, and it does not depend on knowing this one.
	if mf, mferr := loadManifest(m.RepoRoot); mferr == nil {
		if mf.Project == r.Project {
			return fmt.Errorf("bay: %q does not exist", name)
		}
		return fmt.Errorf("bay: %q belongs to project %q, and this repository is %q; run the command from that project",
			name, r.Project, mf.Project)
	}
	return fmt.Errorf("bay: %q belongs to project %q; run the command from that repository", name, r.Project)
}

// Focus moves the canonical hostname to one bay and takes it from the others.
func (m *Manager) Focus(ctx context.Context, name string) error {
	target, ok := m.Get(name)
	if !ok {
		return m.notFound(ctx, name)
	}

	m.mu.Lock()
	others := make([]*Bay, 0, len(m.bays))
	for n, b := range m.bays {
		if n != name {
			others = append(others, b)
		}
	}
	m.mu.Unlock()

	// Unfocus first. Two bays claiming the same hostname is not a state the
	// proxy can resolve sensibly, so it must never exist even briefly.
	for _, b := range others {
		if b.Engine.Focused() {
			if err := b.Engine.Focus(ctx, false); err != nil {
				return err
			}
			_ = m.store.SetFocus(ctx, b.Name, false)
		}
	}
	if err := target.Engine.Focus(ctx, true); err != nil {
		return err
	}
	return m.store.SetFocus(ctx, name, true)
}

// RunTask runs a declared task in a bay.
func (m *Manager) RunTask(ctx context.Context, name, task string) (*engine.TaskResult, error) {
	b, ok := m.Get(name)
	if !ok {
		return nil, m.notFound(ctx, name)
	}
	res, err := b.Engine.RunTask(ctx, task)
	// A task runs a container over the worktree, so it can take ownership of
	// whatever it wrote. Checked after every run, because the next thing the
	// developer does is edit the file the task just complained about.
	m.EnsureWritable(ctx, b.Worktree)
	return res, err
}

// Adopt registers an already-created bay, used when the daemon restarts and
// finds containers it did not start in this process.
func (m *Manager) Adopt(b *Bay) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bays[b.Name] = b
}

// Proxy exposes the shared proxy, or nil when hostname routing is off.
func (m *Manager) Proxy() *proxy.Proxy { return m.prox }

// Scrubber exposes the shared scrubber.
func (m *Manager) Scrubber() *scrub.Scrubber { return m.scrub }

// Secrets exposes the registry that resolves ${secret:...} references.
func (m *Manager) Secrets() *Secrets { return m.secrets }

// Broker exposes the credential broker.
func (m *Manager) Broker() *broker.Broker { return m.broker }

// secretsFor returns the lookup a bay's containers resolve through.
//
// Values set directly on the manager win, because they are what a caller
// explicitly provided; anything else goes to the broker, which may mint a
// credential and will then own its lifetime.
func (m *Manager) secretsFor(bay string) func(string) (string, bool) {
	viaBroker := m.broker.Lookup(bay)
	return func(ref string) (string, bool) {
		if v, ok := m.secrets.Lookup(ref); ok {
			return v, true
		}
		return viaBroker(ref)
	}
}

// SetSecret registers a value, teaching both the resolver and the scrubber
// about it in one call so the two can never disagree.
func (m *Manager) SetSecret(ref, value string) { m.secrets.Set(ref, value) }

// ManifestNames are the files a bay is described by, in precedence order.
var ManifestNames = []string{"devbay.yaml", "devbay.yml"}

func loadManifest(dir string) (*manifest.Manifest, error) {
	return loadManifestFor(dir, "")
}

// loadManifestFor reads a bay's manifest, and explains the usual reason it is
// missing.
//
// A bay is a fresh checkout of a branch, so it contains what is committed and
// nothing else. The common failure is therefore not a missing file but an
// uncommitted one: `devbay init` wrote devbay.yaml, the developer went
// straight to `devbay new`, and the worktree does not have it. Telling them to
// run `devbay init` -- which they just did -- sends them round the loop again.
func loadManifestFor(dir, repoRoot string) (*manifest.Manifest, error) {
	for _, name := range ManifestNames {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return manifest.Load(p)
		}
	}
	if repoRoot != "" {
		for _, name := range ManifestNames {
			if _, err := os.Stat(filepath.Join(repoRoot, name)); err == nil {
				return nil, fmt.Errorf(
					"bay: %s exists in your checkout but is not committed, so this bay's branch does not have it.\n"+
						"      A bay is a fresh checkout, so commit it first:\n"+
						"        git add %s && git commit -m \"add devbay.yaml\"", name, name)
			}
		}
	}
	return nil, fmt.Errorf("bay: no devbay.yaml in %s; run `devbay init` to generate one", dir)
}

// DeriveAlias turns a branch name into a short label.
//
// Agents produce branch names like
// feat/refactor-auth-middleware-to-support-refresh-token-rotation, which is
// useless in a tab strip or an `ls` column. The meaningful head of the name is
// almost always enough to tell two bays apart.
func DeriveAlias(branch string) string {
	s := branch
	// Drop a conventional prefix: "feat/", "fix/", "chore/".
	if i := strings.LastIndex(s, "/"); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		case r == '-' || r == '_' || r == ' ':
			return '-'
		}
		return -1
	}, s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) <= MaxAlias {
		return s
	}
	// Cut at a word boundary when one is near the limit, so the label reads as
	// a word rather than a truncation.
	cut := s[:MaxAlias]
	if i := strings.LastIndex(cut, "-"); i >= MaxAlias/2 {
		cut = cut[:i]
	}
	return strings.Trim(cut, "-")
}

// BootError says a bay exists but something in it did not come up.
//
// A distinct type because the two outcomes need different handling and used to
// be conflated: "the bay could not be created" is a dead end, while "the bay is
// running with one broken service" is the ordinary state of a stack somebody is
// still working on. The bay it names is real -- it will appear in `devbay ls`,
// its logs can be read, its healthy services serve, and `devbay rm` removes it.
type BootError struct {
	Bay string
	Err error
}

func (e *BootError) Error() string {
	return fmt.Sprintf("%s\n\nThe bay was kept so you can look: `devbay logs %s <service>`, "+
		"`devbay status %s`. Its healthy services are serving.", e.Err, e.Bay, e.Bay)
}

func (e *BootError) Unwrap() error { return e.Err }

// Degraded reports whether err is a bay that exists with something broken in it.
func Degraded(err error) (*BootError, bool) {
	var be *BootError
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}
