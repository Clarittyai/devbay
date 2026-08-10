// Package engine brings a bay up and takes it down.
//
// Everything devbay creates in Docker carries the labels in this file. That is
// not bookkeeping: teardown is defined as "remove everything with these
// labels", so completeness is a property of the labelling rather than of
// remembering to delete each thing. A resource devbay creates without a label
// is a resource devbay will leak, which is why creation goes through one
// place.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/egress"
	"github.com/Clarittyai/devbay/internal/manifest"
	"github.com/Clarittyai/devbay/internal/ports"
	"github.com/Clarittyai/devbay/internal/proxy"
	"github.com/Clarittyai/devbay/internal/scrub"
)

// Labels applied to every container, network and volume devbay creates.
const (
	LabelManaged = "dev.devbay.managed"
	LabelProject = "dev.devbay.project"
	LabelBay     = "dev.devbay.bay"
	LabelService = "dev.devbay.service"
)

// WorkspaceDir is where the worktree is mounted inside every container.
const WorkspaceDir = "/workspace"

// loopback is the only address published ports are bound to. Binding to
// 0.0.0.0 would expose every bay to the local network, which is a surprising
// default for a tool whose containers hold real credentials.
var loopback = netip.MustParseAddr("127.0.0.1")

// Engine operates on one bay.
type Engine struct {
	cli      *client.Client
	m        *manifest.Manifest
	bay      string
	worktree string
	res      *Resolver

	alloc    *ports.Allocator
	prox     *proxy.Proxy
	scrubber *scrub.Scrubber
	egress   *egress.Enforcer
	// focused marks this bay as holder of the project's canonical hostname.
	focused bool
	// assigned maps a port key -- "service" or "service/portname" -- to the
	// host port it publishes on. Empty when no allocator is configured, in
	// which case Docker picks ephemeral ports.
	assigned map[string]int

	// Log receives progress lines. Never nil after New.
	Log func(format string, args ...any)
}

// Options configure an Engine.
type Options struct {
	Manifest *manifest.Manifest
	Bay      string
	Worktree string
	Log      func(format string, args ...any)

	// Ports assigns stable host ports. When nil, Docker chooses ephemeral
	// ones, which is fine for a throwaway bay but means a URL does not
	// survive a restart.
	Ports *ports.Allocator

	// Proxy publishes the bay's hostnames. When nil, the bay is reachable at
	// 127.0.0.1:<port> only, which works for agents and probes but gives up
	// the per-bay browser origin.
	Proxy *proxy.Proxy

	// Scrubber removes secret values from anything the engine returns. When
	// nil, a shape-only scrubber is used: returning raw output would be a
	// worse default than a slightly over-eager one.
	Scrubber *scrub.Scrubber

	// Secrets resolves ${secret:path} at container spawn time. When nil, a
	// manifest referencing a secret fails to boot rather than starting with a
	// blank credential, which would fail later and less clearly.
	Secrets func(path string) (string, bool)

	// Egress enforces the per-service network allowlist. When nil, services
	// keep whatever network Docker gives them, which is everything -- so this
	// being nil is a decision, not a default.
	Egress *egress.Enforcer
}

// New connects to Docker and prepares an engine for one bay.
func New(ctx context.Context, opts Options) (*Engine, error) {
	if opts.Manifest == nil || opts.Bay == "" || opts.Worktree == "" {
		return nil, errors.New("engine: manifest, bay and worktree are required")
	}
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("engine: connecting to Docker: %w", err)
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		return nil, fmt.Errorf("engine: Docker is not responding; is the daemon running? %w", err)
	}
	logf := opts.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	e := &Engine{
		cli:      cli,
		m:        opts.Manifest,
		bay:      opts.Bay,
		worktree: opts.Worktree,
		res:      NewResolver(opts.Manifest, opts.Bay),
		alloc:    opts.Ports,
		prox:     opts.Proxy,
		scrubber: opts.Scrubber,
		egress:   opts.Egress,
		assigned: map[string]int{},
		Log:      logf,
	}
	if e.scrubber == nil {
		e.scrubber = scrub.New()
	}
	if opts.Secrets != nil {
		e.res.SetSecrets(opts.Secrets)
	}
	if err := e.assignPorts(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

// PortKeys lists every publishable port in a manifest, sorted.
//
// Sorted, because the mapping from key to port must depend only on what the
// manifest declares. Ranging over the services map directly would reassign
// ports on every boot and quietly undo the determinism the allocator exists to
// provide.
func PortKeys(m *manifest.Manifest) []string {
	var keys []string
	for name, s := range m.Services {
		if s.IsOneshot() {
			continue
		}
		if s.Port != 0 {
			keys = append(keys, name)
		}
		for pn := range s.Ports {
			keys = append(keys, name+"/"+pn)
		}
	}
	sort.Strings(keys)
	return keys
}

// assignPorts reserves a block and maps it onto the manifest's ports.
func (e *Engine) assignPorts(ctx context.Context) error {
	if e.alloc == nil {
		return nil
	}
	keys := PortKeys(e.m)
	if len(keys) == 0 {
		return nil
	}
	block, err := e.alloc.Allocate(ctx, e.m.Project, e.bay, len(keys))
	if err != nil {
		return fmt.Errorf("engine: allocating ports: %w", err)
	}
	e.assigned = block.Assign(keys)

	// The resolver can answer host-plane questions before anything starts,
	// which is what lets devbay print a URL the moment a bay is created.
	for key, hp := range e.assigned {
		if svc, pn, named := strings.Cut(key, "/"); named {
			e.res.SetNamedHostPort(svc, pn, hp)
		} else {
			e.res.SetHostPort(key, hp)
		}
	}
	return nil
}

// Close releases the Docker connection.
func (e *Engine) Close() error { return e.cli.Close() }

// Resolver exposes the address resolver, populated with published ports as
// containers start.
func (e *Engine) Resolver() *Resolver { return e.res }

// prefix namespaces every Docker object belonging to this bay.
func (e *Engine) prefix() string { return "devbay-" + e.m.Project + "-" + e.bay }

func (e *Engine) containerName(service string) string { return e.prefix() + "-" + service }
func (e *Engine) networkName() string                 { return e.prefix() }

// volumeName names a dependency volume. The path is slugified because volume
// names cannot contain slashes.
func (e *Engine) volumeName(service, path string) string {
	slug := strings.NewReplacer("/", "-", ".", "", "_", "-").Replace(strings.Trim(path, "/"))
	return e.prefix() + "-" + service + "-" + slug
}

func (e *Engine) labels(service string) map[string]string {
	l := map[string]string{
		LabelManaged: "1",
		LabelProject: e.m.Project,
		LabelBay:     e.bay,
	}
	if service != "" {
		l[LabelService] = service
	}
	return l
}

// filter matches everything belonging to this bay.
func (e *Engine) filter() client.Filters {
	return make(client.Filters).
		Add("label", LabelManaged+"=1").
		Add("label", LabelProject+"="+e.m.Project).
		Add("label", LabelBay+"="+e.bay)
}

// Up brings the plan's services up, wave by wave.
//
// Services within a wave start concurrently and the wave is not complete until
// every service in it is healthy, so a slow database delays only what depends
// on it. A oneshot completes when it exits zero; a long-running service
// completes when its probe passes.
func (e *Engine) Up(ctx context.Context, plan *Plan) error {
	if len(plan.Steps) == 0 {
		return nil // a task that needs nothing boots nothing
	}
	if err := e.ensureNetwork(ctx); err != nil {
		return err
	}
	// Attached before anything starts, so the proxy can resolve service names
	// the moment they exist.
	if err := e.attachProxy(ctx); err != nil {
		return err
	}

	for i, wave := range plan.Waves() {
		if len(wave) == 0 {
			continue
		}
		names := make([]string, len(wave))
		for j, s := range wave {
			names[j] = s.Service
		}
		e.Log("wave %d: %s", i, strings.Join(names, ", "))

		var wg sync.WaitGroup
		errs := make([]error, len(wave))
		for j, step := range wave {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[j] = e.bring(ctx, step)
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return fmt.Errorf("wave %d: %w", i, err)
		}
	}

	// Routes go up last. Publishing them earlier would give a supervisor a
	// hostname that answers 502 while the bay is still coming up, which reads
	// as a broken app rather than an unfinished boot.
	return e.publishRoutes(ctx)
}

// bring starts one service and waits for it to be ready.
//
// Idempotent, because Up is called far more often than a bay is created: every
// task run calls it to materialize what the task needs, and most of the time
// that work is already done. Re-creating a running container would collide on
// its name, and re-running a completed one-shot would apply a migration twice.
func (e *Engine) bring(ctx context.Context, step Step) error {
	name := step.Service
	s := e.m.Services[name]

	if done, err := e.alreadyUp(ctx, step); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	} else if done {
		return nil
	}

	if err := e.ensureImage(ctx, s); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	// Dependencies are installed in a throwaway container that shares the
	// worktree and the dependency volumes. Running install and start as two
	// separate execs rather than chaining them keeps R1 honest: there is no
	// point at which devbay composes a shell command.
	if len(s.Install) > 0 {
		if err := e.runInstall(ctx, name, s); err != nil {
			return fmt.Errorf("%s: install: %w", name, err)
		}
	}

	id, err := e.create(ctx, name, s, s.Command())
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if _, err := e.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("%s: starting: %w", name, err)
	}

	if step.Oneshot {
		code, err := e.wait(ctx, id)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if code != 0 {
			tail, _ := e.Logs(ctx, name, 40)
			return fmt.Errorf("%s: exited %d\n%s", name, code, tail)
		}
		e.Log("  %s completed", name)
		return nil
	}

	if err := e.applyEgress(ctx, name, s, id); err != nil {
		return err
	}

	if err := e.recordPorts(ctx, name, id); err != nil {
		// A container that exited during startup has no port bindings, so the
		// first symptom is a missing port. Reporting that would name a
		// consequence and hide the cause, leaving an agent to debug the wrong
		// thing, so check for the exit before trusting the port error.
		if alive, code, cerr := e.running(ctx, id); cerr == nil && !alive {
			tail, _ := e.Logs(ctx, name, 40)
			return fmt.Errorf("%s: exited with code %d during startup\n%s", name, code, tail)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := e.waitHealthy(ctx, name, id, s); err != nil {
		tail, _ := e.Logs(ctx, name, 40)
		return fmt.Errorf("%s: %w\n%s", name, err, tail)
	}
	e.Log("  %s healthy", name)
	return nil
}

// alreadyUp reports whether a service needs no further work, adopting an
// existing container where one is usable.
func (e *Engine) alreadyUp(ctx context.Context, step Step) (bool, error) {
	id, err := e.containerID(ctx, step.Service)
	if err != nil {
		return false, nil // no container yet; the normal first-boot path
	}

	ins, err := e.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return false, nil
	}
	st := ins.Container.State
	if st == nil {
		return false, nil
	}

	if step.Oneshot {
		// A one-shot that has already succeeded must not run again: applying a
		// migration twice is at best wasted work and at worst destructive.
		if !st.Running && st.ExitCode == 0 {
			return true, nil
		}
		if st.Running {
			code, err := e.wait(ctx, id)
			if err != nil {
				return false, err
			}
			if code != 0 {
				tail, _ := e.Logs(ctx, step.Service, 40)
				return false, fmt.Errorf("exited %d\n%s", code, tail)
			}
			return true, nil
		}
		// It failed last time. Remove it so this attempt starts clean.
		_ = e.remove(ctx, id)
		return false, nil
	}

	switch {
	case st.Running:
		// Ports are re-read rather than assumed: this process may not be the
		// one that started the container.
		if err := e.recordPorts(ctx, step.Service, id); err != nil {
			return false, err
		}
		return true, nil
	case st.Paused:
		if _, err := e.cli.ContainerUnpause(ctx, id, client.ContainerUnpauseOptions{}); err != nil {
			return false, err
		}
		if err := e.recordPorts(ctx, step.Service, id); err != nil {
			return false, err
		}
		return true, nil
	default:
		// Stopped. Start it again and re-probe, since the process really did
		// exit and a container that starts is not a service that works.
		if _, err := e.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
			// Its configuration may no longer match the manifest; rebuilding
			// is safer than starting something stale.
			_ = e.remove(ctx, id)
			return false, nil
		}
		if err := e.recordPorts(ctx, step.Service, id); err != nil {
			return false, err
		}
		if err := e.waitHealthy(ctx, step.Service, id, e.m.Services[step.Service]); err != nil {
			return false, err
		}
		return true, nil
	}
}

// create builds the container for a service.
func (e *Engine) create(ctx context.Context, name string, s *manifest.Service, cmd manifest.Argv) (string, error) {
	// Container environments are rendered on the container plane, so services
	// address each other over the bay network and never through loopback or a
	// browser hostname.
	env, err := e.res.ResolveEnv(s.Env, PlaneContainer)
	if err != nil {
		return "", err
	}
	envList := make([]string, 0, len(env))
	for k, v := range env {
		envList = append(envList, k+"="+v)
	}
	sort.Strings(envList)

	workdir := s.Workdir
	if workdir == "" {
		workdir = WorkspaceDir
	}

	exposed := network.PortSet{}
	bindings := network.PortMap{}
	addPort := func(containerPort int, key string) error {
		port, err := network.ParsePort(strconv.Itoa(containerPort) + "/tcp")
		if err != nil {
			return err
		}
		exposed[port] = struct{}{}
		b := network.PortBinding{HostIP: loopback}
		// An empty HostPort asks Docker for an ephemeral one. That is only the
		// fallback: with an allocator, the port is stable across restarts, so
		// a bookmarked URL keeps working.
		if hp, ok := e.assigned[key]; ok {
			b.HostPort = strconv.Itoa(hp)
		}
		bindings[port] = []network.PortBinding{b}
		return nil
	}
	if s.Port != 0 {
		if err := addPort(s.Port, name); err != nil {
			return "", err
		}
	}
	for pn, p := range s.Ports {
		if err := addPort(p, name+"/"+pn); err != nil {
			return "", err
		}
	}

	mounts := make([]mount.Mount, 0, len(s.Volumes))
	for _, rel := range s.Volumes {
		vol := e.volumeName(name, rel)
		if err := e.ensureVolume(ctx, name, vol); err != nil {
			return "", err
		}
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: vol,
			Target: workdir + "/" + strings.Trim(rel, "/"),
		})
	}

	res, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: e.containerName(name),
		Config: &container.Config{
			Image:        s.Image,
			Cmd:          cmd,
			Env:          envList,
			WorkingDir:   workdir,
			Labels:       e.labels(name),
			ExposedPorts: exposed,
		},
		HostConfig: &container.HostConfig{
			Binds:        []string{e.worktree + ":" + WorkspaceDir},
			Mounts:       mounts,
			PortBindings: bindings,
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				e.networkName(): {Aliases: []string{name}},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("creating container: %w", err)
	}
	return res.ID, nil
}

// runInstall installs dependencies in a throwaway container.
func (e *Engine) runInstall(ctx context.Context, name string, s *manifest.Service) error {
	argv, enforced := hardenInstall(s.Install, s.InstallScripts)
	if !enforced && !s.InstallScripts {
		// Being unable to disable lifecycle scripts is worth saying out loud.
		// Running them on freshly cloned code is the delivery mechanism the
		// Shai-Hulud npm worm used, and silence here would read as safety.
		e.Log("  %s: cannot disable install scripts for %q; they will run", name, s.Install[0])
	}

	// A copy of the service with the install command and no ports, so it does
	// not race the real container for a published port.
	tmp := *s
	tmp.Port, tmp.Ports = 0, nil
	installName := name + "-install"

	// The container is started idle and the install is exec'd into it
	// afterwards, so the network policy is in place before any of the
	// repository's own code runs.
	//
	// Starting it with the install as its command would leave a window between
	// start and enforcement, and this is the one step where that window
	// matters most: a package lifecycle script runs code devbay has never seen,
	// from a repository it has just cloned, and reaching the network is its
	// entire purpose.
	id, err := e.create(ctx, installName, &tmp, manifest.Argv{"sleep", "3600"})
	if err != nil {
		return err
	}
	defer e.remove(context.WithoutCancel(ctx), id)

	if _, err := e.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return err
	}
	if err := e.applyEgress(ctx, name+" (install)", s, id); err != nil {
		return err
	}

	code, out, err := e.execIn(ctx, id, argv, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exited %d\n%s", code, tail(e.scrubText(out), 4<<10))
	}
	return nil
}

// applyEgress installs a service's network allowlist.
func (e *Engine) applyEgress(ctx context.Context, name string, s *manifest.Service, id string) error {
	if e.egress == nil {
		return nil
	}
	if err := e.egress.Apply(ctx, id, egress.Policy{
		Service: name,
		Allow:   s.Egress,
		// A service that declared any destination is installing something, so
		// the package registries come with it. A service that declared none
		// gets nothing, which is the point.
		AllowDefaults: len(s.Egress) > 0,
	}); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// hardenInstall appends the ecosystem's flag for skipping lifecycle scripts.
// It reports whether it could actually enforce the setting.
func hardenInstall(argv manifest.Argv, allowScripts bool) (manifest.Argv, bool) {
	if allowScripts || len(argv) == 0 {
		return argv, allowScripts
	}
	for _, a := range argv {
		if a == "--ignore-scripts" {
			return argv, true
		}
	}
	switch argv[0] {
	case "npm", "pnpm", "bun", "npx", "pnpx":
		return append(append(manifest.Argv{}, argv...), "--ignore-scripts"), true
	}
	// Yarn Berry uses --mode=skip-build and Yarn 1 uses --ignore-scripts, and
	// guessing wrong turns a security default into a broken install. pip, uv,
	// bundler and go have no equivalent at all. Report honestly rather than
	// pretend.
	return argv, false
}

func (e *Engine) wait(ctx context.Context, id string) (int, error) {
	res := e.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case err := <-res.Error:
		return 0, err
	case st := <-res.Result:
		if st.Error != nil && st.Error.Message != "" {
			return int(st.StatusCode), errors.New(st.Error.Message)
		}
		return int(st.StatusCode), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// recordPorts reads back the ephemeral host ports Docker assigned, so probes
// and agent-facing URLs can use them.
func (e *Engine) recordPorts(ctx context.Context, name, id string) error {
	ins, err := e.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	s := e.m.Services[name]
	find := func(cport int) (int, bool) {
		for p, bindings := range ins.Container.NetworkSettings.Ports {
			if p.Num() != uint16(cport) || len(bindings) == 0 {
				continue
			}
			hp, err := strconv.Atoi(bindings[0].HostPort)
			if err == nil && hp != 0 {
				return hp, true
			}
		}
		return 0, false
	}

	if s.Port != 0 {
		hp, ok := find(s.Port)
		if !ok {
			return fmt.Errorf("container port %d was not published", s.Port)
		}
		e.res.SetHostPort(name, hp)
	}
	for pn, cp := range s.Ports {
		hp, ok := find(cp)
		if !ok {
			return fmt.Errorf("named port %s (%d) was not published", pn, cp)
		}
		e.res.SetNamedHostPort(name, pn, hp)
	}
	return nil
}

func (e *Engine) ensureNetwork(ctx context.Context) error {
	_, err := e.cli.NetworkCreate(ctx, e.networkName(), client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: e.labels(""),
	})
	if err != nil && !isConflict(err) {
		return fmt.Errorf("creating network: %w", err)
	}
	return nil
}

func (e *Engine) ensureVolume(ctx context.Context, service, name string) error {
	_, err := e.cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   name,
		Labels: e.labels(service),
	})
	if err != nil {
		return fmt.Errorf("creating volume %s: %w", name, err)
	}
	return nil
}

func (e *Engine) ensureImage(ctx context.Context, s *manifest.Service) error {
	if s.Image == "" {
		return errors.New("build: is not implemented yet; use image:")
	}
	found, err := e.cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("reference", s.Image),
	})
	if err == nil && len(found.Items) > 0 {
		return nil
	}
	e.Log("  pulling %s", s.Image)
	resp, err := e.cli.ImagePull(ctx, s.Image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pulling %s: %w", s.Image, err)
	}
	defer resp.Close()
	// The body must be drained for the pull to complete.
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pulling %s: %w", s.Image, err)
	}
	return nil
}

// Logs returns the last n lines from a service, as a single string.
func (e *Engine) Logs(ctx context.Context, service string, n int) (string, error) {
	id, err := e.containerID(ctx, service)
	if err != nil {
		return "", err
	}
	return e.logsOf(ctx, id, n)
}

func (e *Engine) logsOf(ctx context.Context, id string, n int) (string, error) {
	rc, err := e.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: strconv.Itoa(n),
	})
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var b strings.Builder
	if _, err := io.Copy(&b, demux(rc)); err != nil {
		return e.scrubText(b.String()), err
	}
	return e.scrubText(b.String()), nil
}

func (e *Engine) containerID(ctx context.Context, service string) (string, error) {
	list, err := e.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: e.filter().Add("label", LabelService+"="+service),
	})
	if err != nil {
		return "", err
	}
	if len(list.Items) == 0 {
		return "", fmt.Errorf("service %q has no container", service)
	}
	return list.Items[0].ID, nil
}

// ServiceStatus is the observable state of one service.
type ServiceStatus struct {
	Service  string
	State    string
	Health   string
	HostPort int
}

// Status reports every container belonging to this bay.
func (e *Engine) Status(ctx context.Context) ([]ServiceStatus, error) {
	list, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: e.filter()})
	if err != nil {
		return nil, err
	}
	out := make([]ServiceStatus, 0, len(list.Items))
	for _, c := range list.Items {
		st := ServiceStatus{Service: c.Labels[LabelService], State: string(c.State)}
		if hp, ok := e.res.hostPortFor(st.Service); ok {
			st.HostPort = hp
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// Down removes every Docker object belonging to this bay.
//
// Teardown is expressed as a label query rather than as a list of things
// devbay remembers creating, because the two can disagree — after a crash, or
// a partial boot — and only the label query is right in both cases. A leaked
// volume or network after teardown is a bug of the same severity as a crash:
// it silently changes the behaviour of the next bay.
func (e *Engine) Down(ctx context.Context) error {
	var errs []error

	// The proxy goes first: its routes must stop resolving before the
	// containers behind them disappear, and it must leave the network before
	// the network can be removed.
	if err := e.releaseProxy(ctx); err != nil {
		errs = append(errs, err)
	}

	list, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: e.filter()})
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	for _, c := range list.Items {
		if err := e.remove(ctx, c.ID); err != nil {
			errs = append(errs, fmt.Errorf("removing container %s: %w", c.Labels[LabelService], err))
		}
	}

	vols, err := e.cli.VolumeList(ctx, client.VolumeListOptions{Filters: e.filter()})
	if err != nil {
		errs = append(errs, fmt.Errorf("listing volumes: %w", err))
	} else {
		for _, v := range vols.Items {
			if _, err := e.cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true}); err != nil {
				errs = append(errs, fmt.Errorf("removing volume %s: %w", v.Name, err))
			}
		}
	}

	// The network goes last, and anything still attached is detached first.
	// Docker refuses to remove a network with active endpoints, and the proxy
	// deliberately attaches to every bay network so it can reach services by
	// name -- so without this, the proxy existing at all would make every
	// teardown report a leak it did not cause.
	if err := e.detachAll(ctx); err != nil {
		errs = append(errs, err)
	}
	if _, err := e.cli.NetworkRemove(ctx, e.networkName(), client.NetworkRemoveOptions{}); err != nil && !isNotFound(err) {
		errs = append(errs, fmt.Errorf("removing network: %w", err))
	}

	if e.alloc != nil {
		// Releasing the port block is part of teardown. Without it the range
		// leaks a block for every bay ever created, and the leak is invisible
		// until allocation starts failing.
		if err := e.alloc.Release(ctx, e.m.Project, e.bay); err != nil {
			errs = append(errs, fmt.Errorf("releasing ports: %w", err))
		}
	}
	return errors.Join(errs...)
}

// detachAll disconnects every container still attached to the bay network.
func (e *Engine) detachAll(ctx context.Context) error {
	ins, err := e.cli.NetworkInspect(ctx, e.networkName(), client.NetworkInspectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspecting network: %w", err)
	}
	var errs []error
	for id := range ins.Network.Containers {
		if _, err := e.cli.NetworkDisconnect(ctx, e.networkName(), client.NetworkDisconnectOptions{
			Container: id, Force: true,
		}); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("detaching %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// NetworkName is the Docker network a bay's containers share. The proxy joins
// it to reach services by name.
func (e *Engine) NetworkName() string { return e.networkName() }

func (e *Engine) remove(ctx context.Context, id string) error {
	_, err := e.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{
		Force:         true, // running containers are stopped, not refused
		RemoveVolumes: true, // anonymous volumes the image declared
	})
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// The Docker client classifies its errors with containerd's errdefs, so these
// ask what the error IS rather than matching on how it happens to be worded.
// Teardown depends on telling "already gone" apart from "failed to remove",
// and a daemon that rephrases a message must not turn a clean teardown into a
// reported leak.
func isNotFound(err error) bool { return cerrdefs.IsNotFound(err) }

func isConflict(err error) bool {
	return cerrdefs.IsConflict(err) || cerrdefs.IsAlreadyExists(err)
}

// parseDuration converts a manifest duration, which the schema restricts to
// ms, s and m.
func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}
