package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// Plane is the vantage point an address is resolved for.
//
// A service has more than one address and they are not interchangeable. This
// is the detail most likely to be got wrong, and getting it wrong produces the
// specific, confusing failure where a page renders in the browser but
// server-side rendering of the same page fails.
type Plane int

const (
	// PlaneContainer is one container addressing another over the bay's
	// network, by service name. Ports are the ones the app actually listens
	// on, so nothing inside a bay needs port-offset awareness.
	PlaneContainer Plane = iota

	// PlaneHost is the daemon, the CLI, or an agent addressing a container
	// from outside, as 127.0.0.1:<published port>. Health probes always use
	// this plane.
	PlaneHost

	// PlaneBrowser is a browser addressing a container by hostname through the
	// proxy. Only ever produced for ${bay.<svc>.public_url}.
	//
	// Never usable by the daemon or by an application: *.localhost does not
	// resolve through getaddrinfo, so Go, Node, Python and Safari all fail on
	// it. Only Chrome, Firefox and curl special-case those names.
	PlaneBrowser
)

// Endpoint is where one service can be reached from a given plane.
type Endpoint struct {
	Host string
	Port int
}

// Addr renders the endpoint as host:port.
func (e Endpoint) Addr() string { return hostPort(e.Host, e.Port) }

func hostPort(host string, port int) string {
	if port == 0 {
		return host
	}
	return host + ":" + strconv.Itoa(port)
}

// Resolver turns ${bay...} and ${secret:...} references into concrete values.
//
// It is constructed per bay, after ports are known, and answers differently
// depending on which plane is asking.
type Resolver struct {
	// Bay and Project form the hostname namespace.
	Bay     string
	Project string
	// TLD is the browser-facing suffix; "localhost" by default.
	TLD string
	// Scheme is https once the proxy has a trusted certificate.
	Scheme string

	m *manifest.Manifest

	// mu guards everything below it.
	//
	// Services in a boot wave start concurrently, and each records its
	// published ports as soon as it is up, so these maps are written from
	// several goroutines at once. Without this the Go runtime would
	// eventually abort the process with "concurrent map writes" -- during a
	// boot, which is the worst possible moment.
	mu sync.RWMutex
	// hostPorts maps service name to the host port its primary container port
	// is published on. Empty until containers exist.
	hostPorts map[string]int
	// namedHostPorts maps "service/portname" the same way.
	namedHostPorts map[string]int

	// secrets resolves ${secret:path}. Nil means unresolved references are an
	// error, which is what validation and dry-run use.
	secrets func(path string) (string, bool)
}

// NewResolver builds a resolver for a bay.
func NewResolver(m *manifest.Manifest, bay string) *Resolver {
	return &Resolver{
		Bay:            bay,
		Project:        m.Project,
		TLD:            "localhost",
		Scheme:         "http",
		m:              m,
		hostPorts:      map[string]int{},
		namedHostPorts: map[string]int{},
	}
}

// SetHostPort records the host port a service's primary port was published on.
func (r *Resolver) SetHostPort(service string, port int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hostPorts[service] = port
}

// SetNamedHostPort records the host port for one of a service's named ports.
func (r *Resolver) SetNamedHostPort(service, name string, port int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.namedHostPorts[service+"/"+name] = port
}

// SetSecrets installs the secret lookup. Kept as a function rather than a map
// so that values are fetched at spawn time and never held longer than needed.
func (r *Resolver) SetSecrets(f func(path string) (string, bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = f
}

// hostPort reads a published primary port.
func (r *Resolver) hostPortFor(service string) (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.hostPorts[service]
	return p, ok
}

// namedHostPort reads a published secondary port.
func (r *Resolver) namedHostPort(service, name string) (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.namedHostPorts[service+"/"+name]
	return p, ok
}

// secretLookup reads the configured secret source.
func (r *Resolver) secretLookup() func(string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.secrets
}

// Hostname returns the browser-facing name for a service.
//
// The primary service claims the bare <bay>.<project>.<tld>; every other
// service is prefixed. Distinct hostnames are what give each bay its own
// browser origin, which is what stops two bays from sharing a cookie jar --
// browsers key cookies by host and ignore the port, so without this, two bays
// of the same app overwrite each other's sessions.
func (r *Resolver) Hostname(service string) string {
	base := r.Bay + "." + r.Project + "." + r.TLD
	if service == r.m.PrimaryService() {
		return base
	}
	return service + "." + base
}

// NamedHostname returns the browser-facing name for a secondary port.
func (r *Resolver) NamedHostname(service, port string) string {
	return port + "." + r.Hostname(service)
}

// Endpoint returns where service can be reached from plane.
func (r *Resolver) Endpoint(service string, plane Plane) (Endpoint, error) {
	s, ok := r.m.Services[service]
	if !ok {
		return Endpoint{}, fmt.Errorf("unknown service %q", service)
	}
	switch plane {
	case PlaneContainer:
		// Docker's embedded DNS resolves the service name on the bay network.
		return Endpoint{Host: service, Port: s.Port}, nil
	case PlaneHost:
		p, ok := r.hostPortFor(service)
		if !ok {
			return Endpoint{}, fmt.Errorf("service %q has no published host port yet", service)
		}
		return Endpoint{Host: "127.0.0.1", Port: p}, nil
	case PlaneBrowser:
		return Endpoint{Host: r.Hostname(service)}, nil
	}
	return Endpoint{}, fmt.Errorf("unknown plane %d", plane)
}

// refPattern matches one permitted reference. The grammar is enforced by the
// manifest validator; this only locates and decomposes.
var refPattern = regexp.MustCompile(
	`\$\{(?:bay\.([a-z0-9-]+)\.(url|public_url|host|port|name|user|password|ports\.[a-z0-9-]+)|secret:([A-Za-z0-9/_.:-]+))\}`)

// ResolveEnv renders a service's environment for the given plane.
//
// Container environments are rendered with PlaneContainer, so a service
// talking to another uses the container network. Values an agent or the CLI
// sees are rendered with PlaneHost. public_url always renders as the browser
// origin regardless of plane, because that is what it means.
func (r *Resolver) ResolveEnv(env map[string]string, plane Plane) (map[string]string, error) {
	out := make(map[string]string, len(env))
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v, err := r.ResolveString(env[k], plane)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

// ResolveString expands every reference in v.
func (r *Resolver) ResolveString(v string, plane Plane) (string, error) {
	var firstErr error
	out := refPattern.ReplaceAllStringFunc(v, func(match string) string {
		g := refPattern.FindStringSubmatch(match)
		svc, field, secret := g[1], g[2], g[3]

		if secret != "" {
			lookup := r.secretLookup()
			if lookup == nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("secret %q referenced but no secret source is configured", secret)
				}
				return match
			}
			val, ok := lookup(secret)
			if !ok {
				if firstErr == nil {
					firstErr = fmt.Errorf("secret %q is not available", secret)
				}
				return match
			}
			return val
		}

		val, err := r.field(svc, field, plane)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return val
	})
	return out, firstErr
}

func (r *Resolver) field(svc, field string, plane Plane) (string, error) {
	s, ok := r.m.Services[svc]
	if !ok {
		return "", fmt.Errorf("unknown service %q", svc)
	}

	if name, isNamed := strings.CutPrefix(field, "ports."); isNamed {
		cp, ok := s.Ports[name]
		if !ok {
			return "", fmt.Errorf("service %q has no named port %q", svc, name)
		}
		if plane == PlaneHost {
			hp, ok := r.namedHostPorts[svc+"/"+name]
			if !ok {
				return "", fmt.Errorf("named port %s/%s is not published yet", svc, name)
			}
			return strconv.Itoa(hp), nil
		}
		return strconv.Itoa(cp), nil
	}

	switch field {
	case "public_url":
		// Always the browser origin, whoever is asking.
		return r.Scheme + "://" + r.Hostname(svc), nil
	case "host":
		ep, err := r.Endpoint(svc, plane)
		return ep.Host, err
	case "port":
		ep, err := r.Endpoint(svc, plane)
		if err != nil {
			return "", err
		}
		return strconv.Itoa(ep.Port), nil
	case "name":
		return dbName(s, svc), nil
	case "user":
		return dbUser(s), nil
	case "password":
		return dbPassword(s), nil
	case "url":
		return r.url(svc, s, plane)
	}
	return "", fmt.Errorf("unknown field %q", field)
}

// url builds a connection string. For a datastore that means a DSN including
// credentials and database name, because that is the single value applications
// actually consume -- DATABASE_URL, REDIS_URL. The credentials come from the
// target service's own environment, using the conventions the official images
// already define, so nothing has to be stated twice.
func (r *Resolver) url(svc string, s *manifest.Service, plane Plane) (string, error) {
	ep, err := r.Endpoint(svc, plane)
	if err != nil {
		return "", err
	}
	scheme := schemeFor(s)

	var auth string
	if u := dbUser(s); u != "" {
		auth = u
		if p := dbPassword(s); p != "" {
			auth += ":" + p
		}
		auth += "@"
	}

	var path string
	if scheme == "postgres" || scheme == "mysql" {
		if n := dbName(s, svc); n != "" {
			path = "/" + n
		}
	}
	return scheme + "://" + auth + ep.Addr() + path, nil
}

// schemeFor picks a URL scheme from the image name. The table is deliberately
// short and covers the official images that appear in a services block; the
// fallback is http, which is right for anything application-shaped.
func schemeFor(s *manifest.Service) string {
	img := s.Image
	if i := strings.IndexAny(img, ":@"); i >= 0 {
		img = img[:i]
	}
	if i := strings.LastIndex(img, "/"); i >= 0 {
		img = img[i+1:]
	}
	switch {
	case strings.HasPrefix(img, "postgres"), strings.HasPrefix(img, "pgvector"), strings.HasPrefix(img, "timescale"):
		return "postgres"
	case strings.HasPrefix(img, "mysql"), strings.HasPrefix(img, "mariadb"):
		return "mysql"
	case strings.HasPrefix(img, "redis"), strings.HasPrefix(img, "valkey"):
		return "redis"
	case strings.HasPrefix(img, "mongo"):
		return "mongodb"
	case strings.HasPrefix(img, "rabbitmq"):
		return "amqp"
	case strings.HasPrefix(img, "nats"):
		return "nats"
	}
	return "http"
}

// The official images agree on how they are configured; these read the answer
// the manifest already gave rather than asking for it again.
func dbUser(s *manifest.Service) string {
	return firstEnv(s, "POSTGRES_USER", "MYSQL_USER", "MONGO_INITDB_ROOT_USERNAME", "RABBITMQ_DEFAULT_USER", "MINIO_ROOT_USER")
}

func dbPassword(s *manifest.Service) string {
	return firstEnv(s, "POSTGRES_PASSWORD", "MYSQL_PASSWORD", "MONGO_INITDB_ROOT_PASSWORD", "RABBITMQ_DEFAULT_PASS", "MINIO_ROOT_PASSWORD")
}

func dbName(s *manifest.Service, fallback string) string {
	if n := firstEnv(s, "POSTGRES_DB", "MYSQL_DATABASE", "MONGO_INITDB_DATABASE"); n != "" {
		return n
	}
	return fallback
}

func firstEnv(s *manifest.Service, keys ...string) string {
	for _, k := range keys {
		if v, ok := s.Env[k]; ok && v != "" {
			return v
		}
	}
	return ""
}
