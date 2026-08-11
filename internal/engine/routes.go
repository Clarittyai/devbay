package engine

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/Clarittyai/devbay/internal/proxy"
)

// publishRoutes tells the proxy how to reach this bay.
//
// Upstreams are container-network addresses, not published host ports, because
// the proxy joins the bay network. That is not a stylistic choice: host ports
// are bound to loopback, and a container cannot reach the host's loopback. The
// alternative -- publishing on all interfaces so the proxy could reach them --
// would expose every bay to the local network.
func (e *Engine) publishRoutes(ctx context.Context) error {
	if e.prox == nil {
		return nil
	}

	var routes []proxy.Route
	names := make([]string, 0, len(e.m.Services))
	for name := range e.m.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	primary := e.m.PrimaryService()
	for _, name := range names {
		s := e.m.Services[name]
		if s.IsOneshot() || s.Port == 0 {
			continue
		}
		routes = append(routes, proxy.Route{
			Host: e.res.Hostname(name),
			// The container's full name, not the service alias. Every bay
			// aliases its containers by service name, and the proxy is joined
			// to every bay's network, so "vote:80" resolves to whichever bay's
			// vote container Docker feels like -- and one bay's hostname
			// silently served another bay's containers. Two bays, two votes,
			// both landing in the same database.
			//
			// That is the isolation devbay exists to provide, so the upstream
			// has to name something that exists exactly once on the machine.
			Upstream: e.containerName(name) + ":" + strconv.Itoa(s.Port),
		})

		// Secondary ports get their own subdomain, so a mail catcher's web UI
		// and its SMTP port are separately addressable rather than one of them
		// being unreachable.
		for pn, cp := range s.Ports {
			routes = append(routes, proxy.Route{
				Host:     e.res.NamedHostname(name, pn),
				Upstream: e.containerName(name) + ":" + strconv.Itoa(cp),
			})
		}

		// The focused bay also answers on the project's canonical hostname.
		// Named hostnames handle most work, but an OAuth redirect URI a
		// provider will not wildcard, a mobile simulator, or a native app
		// config cannot be talked out of a fixed address -- focus is what
		// serves those, and only one bay can hold it at a time.
		if e.focused && name == primary {
			routes = append(routes, proxy.Route{
				Host:     e.m.Project + "." + e.res.TLD,
				Upstream: e.containerName(name) + ":" + strconv.Itoa(s.Port),
			})
		}
	}

	if err := e.prox.SetRoutes(ctx, e.m.Project, e.bay, routes); err != nil {
		return fmt.Errorf("engine: publishing routes: %w", err)
	}
	return nil
}

// Republish re-joins the proxy to this bay's network and re-announces its
// routes.
//
// For the case where the proxy container is newer than the bay: a machine
// restart, a `docker rm`, an upgrade. The bay is still running and still
// answers on its published ports, but the proxy that knew how to reach it is
// gone, so the hostname a developer has bookmarked would 404 with no way back
// short of recreating the bay.
func (e *Engine) Republish(ctx context.Context) error {
	if e.prox == nil {
		return nil
	}
	if err := e.attachProxy(ctx); err != nil {
		return err
	}
	return e.publishRoutes(ctx)
}

// attachProxy joins the proxy to this bay's network.
func (e *Engine) attachProxy(ctx context.Context) error {
	if e.prox == nil {
		return nil
	}
	if err := e.prox.Attach(ctx, e.networkName()); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	return nil
}

// releaseProxy withdraws this bay's routes and leaves its network.
//
// Both halves matter. Leaving the routes behind would keep a hostname
// resolving to a container that no longer exists; staying attached would make
// the network un-removable and turn every teardown into a reported leak.
func (e *Engine) releaseProxy(ctx context.Context) error {
	if e.prox == nil {
		return nil
	}
	var errs []error
	if err := e.prox.ClearRoutes(ctx, e.m.Project, e.bay); err != nil {
		errs = append(errs, err)
	}
	if err := e.prox.Detach(ctx, e.networkName()); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("engine: releasing proxy: %v", errs)
	}
	return nil
}

// URLs returns the browser-facing address of every routable service.
func (e *Engine) URLs() map[string]string {
	out := map[string]string{}
	scheme := e.res.Scheme + "://"
	suffix := ""
	if e.prox != nil && e.prox.HTTPPort != 0 && e.prox.HTTPPort != 80 {
		// Without :80 the hostname alone is not enough, and printing a URL
		// that does not work is worse than printing a longer one.
		suffix = ":" + strconv.Itoa(e.prox.HTTPPort)
	}
	for name, s := range e.m.Services {
		if s.IsOneshot() || s.Port == 0 {
			continue
		}
		out[name] = scheme + e.res.Hostname(name) + suffix
		for pn := range s.Ports {
			out[name+"/"+pn] = scheme + e.res.NamedHostname(name, pn) + suffix
		}
	}
	return out
}
