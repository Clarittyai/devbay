// Package proxy gives every bay its own browser origin.
//
// This is a correctness feature, not cosmetics. Browsers scope cookies,
// localStorage, IndexedDB and service worker registrations by host and
// deliberately ignore the port, so two bays served from localhost:3000 and
// localhost:3001 share one storage partition. Log into one and you are logged
// into the other; log out of one and the other's session evaporates. The
// resulting bugs only reproduce when two bays are running, which is exactly
// when a developer is least able to reason about them.
//
// Distinct hostnames eliminate the whole class.
//
// Two implementation notes explain the shape of this package.
//
// The proxy runs as a container rather than in the daemon because macOS has no
// setcap and no unprivileged-port sysctl, and pf rules need root and do not
// survive a reboot. Publishing :80 from a container makes Docker's already-root
// helper perform the privileged bind, so devbay itself never needs sudo.
//
// The proxy joins each bay's network rather than reaching services through the
// host, because published ports are bound to loopback and a container cannot
// reach the host's loopback. Publishing on 0.0.0.0 to make that work would put
// every bay, and every credential it holds, on the local network.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	// Image is the proxy image. Caddy is used rather than a hand-written
	// reverse proxy because WebSocket upgrades, HTTP/2, and a local
	// certificate authority are all requirements here and all solved problems
	// there.
	Image = "caddy:2-alpine"

	// ContainerName is shared by every bay; there is one proxy per machine.
	ContainerName = "devbay-proxy"

	// AdminPort is Caddy's config API inside the container.
	AdminPort = 2019

	// LabelManaged marks the proxy container.
	LabelManaged = "dev.devbay.proxy"
)

// Route sends one hostname to one container port.
type Route struct {
	// Host is the full hostname, e.g. add-oauth.acme.localhost.
	Host string
	// Upstream is the container-network address, e.g. web:3000. The proxy
	// resolves it over the bay network it is attached to.
	Upstream string
	// Bay and Project identify the owner, so routes can be replaced per bay.
	Project string
	Bay     string
}

// Proxy manages the shared reverse proxy container.
type Proxy struct {
	cli *client.Client

	// HTTPPort is the host port :80 is published on. It falls back to 8080
	// when 80 cannot be bound, which keeps devbay usable rather than refusing
	// to start.
	HTTPPort int
	// adminPort is the host port Caddy's admin API is published on.
	adminPort int

	routes map[string]Route // keyed by host
	nets   map[string]bool  // bay networks currently attached

	Log func(format string, args ...any)
}

// New prepares a proxy handle. It does not start anything.
func New(cli *client.Client, logf func(string, ...any)) *Proxy {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Proxy{
		cli:    cli,
		routes: map[string]Route{},
		nets:   map[string]bool{},
		Log:    logf,
	}
}

// Ensure starts the proxy if it is not already running.
//
// httpPort is the host port to serve on; 0 means try 80 and fall back to 8080.
// adminPort is where Caddy's config API is published, on loopback only.
func (p *Proxy) Ensure(ctx context.Context, httpPort, adminPort int) error {
	if adminPort == 0 {
		adminPort = 2019
	}
	p.adminPort = adminPort

	if id, running, err := p.find(ctx); err != nil {
		return err
	} else if id != "" {
		// A proxy left running by another process is reused, but only if it is
		// actually reachable on the admin port this caller expects. Assuming
		// the requested port were the published one would leave every route
		// push failing against a port nothing is listening on -- and the
		// symptom appears later, as a bay that boots and then 404s.
		httpPort, adminPort := p.publishedPorts(ctx, id)
		if running && adminPort == p.adminPort {
			p.HTTPPort = httpPort
			return p.syncRoutes(ctx)
		}
		if running {
			p.Log("proxy: replacing a proxy published on admin port %d; this run needs %d", adminPort, p.adminPort)
		}
		// A stopped proxy from a previous run is replaced rather than
		// restarted: its published ports and admin port may no longer match
		// what this run wants.
		if _, err := p.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("proxy: removing stale container: %w", err)
		}
	}

	// Pulled before the bind loop rather than inside it, so a missing image is
	// reported once as a missing image instead of once per candidate port as a
	// container that would not start.
	if err := p.ensureImage(ctx); err != nil {
		return err
	}

	candidates := []int{httpPort}
	if httpPort == 0 {
		// Port 80 is what makes a bare hostname work with no port suffix.
		// Falling back rather than failing keeps devbay usable on a machine
		// where something already owns it.
		candidates = []int{80, 8080}
	}

	var lastErr error
	for _, port := range candidates {
		id, err := p.create(ctx, port, adminPort)
		if err != nil {
			lastErr = err
			continue
		}
		if _, err := p.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
			lastErr = err
			_, _ = p.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
			continue
		}
		p.HTTPPort = port
		if port != 80 && len(candidates) > 1 {
			// Only worth saying when devbay actually fell back; a caller that
			// asked for a specific port already knows which one it chose.
			p.Log("proxy: could not bind :80, serving on :%d — bay URLs need the port suffix", port)
		}
		if err := p.waitAdmin(ctx); err != nil {
			return err
		}
		return p.syncRoutes(ctx)
	}
	return fmt.Errorf("proxy: could not start on any of %v: %w", candidates, lastErr)
}

// ensureImage pulls the proxy image when the machine does not have it.
//
// Every other image devbay runs comes from a manifest and is pulled by the
// engine; this one is devbay's own and was assumed to be present, which is
// true on any machine that has run devbay before and false on a fresh one --
// so the first run on a clean machine failed with "No such image" from deep
// inside a port-binding retry loop.
func (p *Proxy) ensureImage(ctx context.Context) error {
	found, err := p.cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("reference", Image),
	})
	if err == nil && len(found.Items) > 0 {
		return nil
	}
	p.Log("proxy: pulling %s", Image)
	resp, err := p.cli.ImagePull(ctx, Image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("proxy: pulling %s: %w", Image, err)
	}
	defer resp.Close()
	// The body must be drained for the pull to complete.
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("proxy: pulling %s: %w", Image, err)
	}
	return nil
}

func (p *Proxy) create(ctx context.Context, httpPort, adminPort int) (string, error) {
	httpP, err := network.ParsePort(strconv.Itoa(80) + "/tcp")
	if err != nil {
		return "", err
	}
	adminP, err := network.ParsePort(strconv.Itoa(AdminPort) + "/tcp")
	if err != nil {
		return "", err
	}

	res, err := p.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: ContainerName,
		Config: &container.Config{
			Image: Image,
			// Started with no config at all and driven entirely through the
			// admin API, so a route change never restarts the proxy and never
			// drops a WebSocket. Passing an empty config file instead fails:
			// the caddyfile adapter rejects empty input.
			Cmd:    []string{"caddy", "run"},
			Labels: map[string]string{LabelManaged: "1"},
			ExposedPorts: network.PortSet{
				httpP:  struct{}{},
				adminP: struct{}{},
			},
			Env: []string{
				// Bind the admin API to all interfaces *inside* the container,
				// because a port published from the container cannot reach a
				// listener bound to the container's own loopback. It is still
				// only published on the host's loopback.
				//
				// Caddy disables its origin check when the admin endpoint is
				// on an open interface, so the only thing protecting it is that
				// loopback binding. That is the same trust boundary as the
				// Docker socket this process already holds, but it is the
				// reason the admin port must never be published on 0.0.0.0.
				"CADDY_ADMIN=0.0.0.0:" + strconv.Itoa(AdminPort),
			},
		},
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{
				httpP:  []network.PortBinding{{HostIP: anyAddr, HostPort: strconv.Itoa(httpPort)}},
				adminP: []network.PortBinding{{HostIP: loopbackAddr, HostPort: strconv.Itoa(adminPort)}},
			},
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		},
	})
	if err != nil {
		return "", fmt.Errorf("proxy: creating container: %w", err)
	}
	return res.ID, nil
}

func (p *Proxy) find(ctx context.Context) (id string, running bool, err error) {
	list, err := p.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelManaged+"=1"),
	})
	if err != nil {
		return "", false, err
	}
	if len(list.Items) == 0 {
		return "", false, nil
	}
	c := list.Items[0]
	return c.ID, c.State == "running", nil
}

// publishedPorts reads back the host ports an existing proxy container is
// actually published on, which is the only trustworthy source when this
// process did not start it.
func (p *Proxy) publishedPorts(ctx context.Context, id string) (httpPort, adminPort int) {
	ins, err := p.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return 0, 0
	}
	for port, bindings := range ins.Container.NetworkSettings.Ports {
		if len(bindings) == 0 {
			continue
		}
		n, err := strconv.Atoi(bindings[0].HostPort)
		if err != nil {
			continue
		}
		switch port.Num() {
		case 80:
			httpPort = n
		case AdminPort:
			adminPort = n
		}
	}
	return httpPort, adminPort
}

// Attach joins the proxy to a bay's network so it can reach services by name.
func (p *Proxy) Attach(ctx context.Context, networkName string) error {
	if p.nets[networkName] {
		return nil
	}
	_, err := p.cli.NetworkConnect(ctx, networkName, client.NetworkConnectOptions{
		Container: ContainerName,
	})
	// Already-attached is success: Ensure may have found a proxy left running
	// from a previous devbay process that had already joined this network.
	if err != nil && !cerrdefs.IsConflict(err) && !cerrdefs.IsAlreadyExists(err) &&
		!strings.Contains(err.Error(), "already exists in network") {
		return fmt.Errorf("proxy: attaching to %s: %w", networkName, err)
	}
	p.nets[networkName] = true
	return nil
}

// Detach leaves a bay's network. Teardown must call this before removing the
// network, or Docker refuses because the endpoint is still active.
func (p *Proxy) Detach(ctx context.Context, networkName string) error {
	_, err := p.cli.NetworkDisconnect(ctx, networkName, client.NetworkDisconnectOptions{
		Container: ContainerName, Force: true,
	})
	delete(p.nets, networkName)
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("proxy: detaching from %s: %w", networkName, err)
	}
	return nil
}

// SetRoutes replaces every route belonging to one bay.
//
// Replacing rather than merging means a service removed from a manifest stops
// being routable, instead of lingering as a hostname that resolves to a
// container that no longer exists.
func (p *Proxy) SetRoutes(ctx context.Context, project, bay string, routes []Route) error {
	for host, r := range p.routes {
		if r.Project == project && r.Bay == bay {
			delete(p.routes, host)
		}
	}
	for _, r := range routes {
		r.Project, r.Bay = project, bay
		p.routes[r.Host] = r
	}
	return p.syncRoutes(ctx)
}

// ClearRoutes removes a bay's routes.
func (p *Proxy) ClearRoutes(ctx context.Context, project, bay string) error {
	return p.SetRoutes(ctx, project, bay, nil)
}

// Routes returns the current routing table, sorted by host.
func (p *Proxy) Routes() []Route {
	out := make([]Route, 0, len(p.routes))
	for _, r := range p.routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// syncRoutes pushes the whole routing table to Caddy in one atomic load.
func (p *Proxy) syncRoutes(ctx context.Context) error {
	if p.adminPort == 0 {
		return nil // proxy not started
	}
	cfg, err := json.Marshal(p.caddyConfig())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/load", p.adminPort), bytes.NewReader(cfg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("proxy: loading config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("proxy: config rejected (%s): %s", resp.Status, body)
	}
	return nil
}

// caddyConfig renders the routing table as a Caddy JSON config.
func (p *Proxy) caddyConfig() map[string]any {
	routes := make([]any, 0, len(p.routes))
	for _, r := range p.Routes() {
		routes = append(routes, map[string]any{
			"match": []any{map[string]any{"host": []string{r.Host}}},
			"handle": []any{map[string]any{
				"handler":   "reverse_proxy",
				"upstreams": []any{map[string]any{"dial": r.Upstream}},
				"headers": map[string]any{
					"request": map[string]any{
						"set": map[string][]string{
							// Without these the application builds absolute
							// URLs from the upstream address and redirects the
							// browser off the bay origin, which would undo the
							// isolation this package exists for.
							"X-Forwarded-Host":  {"{http.request.host}"},
							"X-Forwarded-Proto": {"{http.request.scheme}"},
						},
					},
				},
			}},
			"terminal": true,
		})
	}
	// A catch-all, last and terminal. Caddy answers an unmatched host with a
	// blank 200, which is the worst possible response here: a supervisor who
	// opens a torn-down bay's URL, or mistypes one, sees an empty page and
	// reads it as their application being broken rather than as the bay not
	// existing. Saying so explicitly turns a debugging session into a glance.
	routes = append(routes, map[string]any{
		"handle": []any{map[string]any{
			"handler":     "static_response",
			"status_code": 404,
			"headers": map[string][]string{
				"Content-Type": {"text/plain; charset=utf-8"},
				"X-Devbay":     {"no-such-bay"},
			},
			"body": "devbay: no bay is serving this hostname.\n\n" +
				"It may have been torn down, may not have finished booting, or the name may be misspelled.\n" +
				"Run `devbay ls` to see what is running.\n",
		}},
		"terminal": true,
	})

	return map[string]any{
		"admin": map[string]any{"listen": "0.0.0.0:" + strconv.Itoa(AdminPort)},
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{
					"devbay": map[string]any{
						"listen": []string{":80"},
						"routes": routes,
					},
				},
			},
		},
	}
}

func (p *Proxy) waitAdmin(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("http://127.0.0.1:%d/config/", p.adminPort), nil)
		if err != nil {
			return err
		}
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("proxy: admin API did not come up on 127.0.0.1:%d", p.adminPort)
}

// Stop removes the proxy container.
func (p *Proxy) Stop(ctx context.Context) error {
	id, _, err := p.find(ctx)
	if err != nil || id == "" {
		return err
	}
	if _, err := p.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	p.routes = map[string]Route{}
	p.nets = map[string]bool{}
	return nil
}
