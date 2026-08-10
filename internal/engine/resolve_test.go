package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Clarittyai/devbay/internal/manifest"
)

func fixture(t *testing.T, name string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Load(filepath.Join("../../testdata/repos", name, "devbay.yaml"))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if r := manifest.Validate(m); !r.OK() {
		t.Fatalf("%s: %v", name, r.Err())
	}
	return m
}

// The address planes are the whole point of the resolver. A service reached
// from inside the bay, from the host, and from a browser has three different
// addresses, and substituting one for another is the single most likely bug.
func TestAddressPlanes(t *testing.T) {
	m := fixture(t, "documenso")
	r := NewResolver(m, "add-oauth")
	r.SetHostPort("web", 40123)
	r.SetHostPort("db", 40124)

	for _, c := range []struct {
		name  string
		svc   string
		plane Plane
		want  Endpoint
	}{
		{"container to container uses the service name and the real port",
			"db", PlaneContainer, Endpoint{Host: "db", Port: 5432}},
		{"host uses loopback and the published port",
			"db", PlaneHost, Endpoint{Host: "127.0.0.1", Port: 40124}},
		{"browser uses the bay hostname",
			"web", PlaneBrowser, Endpoint{Host: "add-oauth.documenso.localhost"}},
	} {
		got, err := r.Endpoint(c.svc, c.plane)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s:\n  got  %+v\n  want %+v", c.name, got, c.want)
		}
	}
}

// The primary service claims the bare bay hostname; everything else is
// prefixed. Distinct origins are what stop two bays sharing a cookie jar.
func TestHostnames(t *testing.T) {
	m := fixture(t, "documenso")
	r := NewResolver(m, "add-oauth")

	if got, want := r.Hostname("web"), "add-oauth.documenso.localhost"; got != want {
		t.Errorf("primary hostname = %q, want %q", got, want)
	}
	if got, want := r.Hostname("storage"), "storage.add-oauth.documenso.localhost"; got != want {
		t.Errorf("secondary hostname = %q, want %q", got, want)
	}
	if got, want := r.NamedHostname("storage", "console"), "console.storage.add-oauth.documenso.localhost"; got != want {
		t.Errorf("named-port hostname = %q, want %q", got, want)
	}

	// Two bays of the same project must never collide, or the isolation is
	// decorative.
	other := NewResolver(m, "fix-login")
	if r.Hostname("web") == other.Hostname("web") {
		t.Error("two bays produced the same origin")
	}
}

// A DATABASE_URL has to be a usable connection string, not just host:port.
// The credentials come from the target service's own environment, using the
// conventions the official images already define.
func TestURLsAreConnectionStrings(t *testing.T) {
	m := fixture(t, "documenso")
	r := NewResolver(m, "b1")
	r.SetHostPort("db", 40100)
	r.SetHostPort("redis", 40101)
	r.SetHostPort("storage", 40102)

	for _, c := range []struct {
		svc   string
		plane Plane
		want  string
	}{
		{"db", PlaneContainer, "postgres://documenso:password@db:5432/documenso"},
		{"db", PlaneHost, "postgres://documenso:password@127.0.0.1:40100/documenso"},
		{"redis", PlaneContainer, "redis://redis:6379"},
		{"storage", PlaneContainer, "http://documenso:password@storage:9002"},
	} {
		got, err := r.ResolveString("${bay."+c.svc+".url}", c.plane)
		if err != nil {
			t.Fatalf("%s: %v", c.svc, err)
		}
		if got != c.want {
			t.Errorf("%s url on plane %d:\n  got  %s\n  want %s", c.svc, c.plane, got, c.want)
		}
	}
}

// public_url means the browser origin no matter who asks. Everything else
// follows the plane.
func TestPublicURLIgnoresPlane(t *testing.T) {
	m := fixture(t, "documenso")
	r := NewResolver(m, "b1")
	r.SetHostPort("web", 40200)

	const want = "http://add-oauth.documenso.localhost"
	r.Bay = "add-oauth"
	for _, plane := range []Plane{PlaneContainer, PlaneHost, PlaneBrowser} {
		got, err := r.ResolveString("${bay.web.public_url}", plane)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("plane %d: public_url = %q, want %q", plane, got, want)
		}
	}
}

// The documenso fixture carries both forms deliberately: an SSR-only URL and a
// browser-exposed one. Rendering a container environment must keep them apart.
func TestResolveEnvKeepsPlanesApart(t *testing.T) {
	m := fixture(t, "documenso")
	r := NewResolver(m, "add-oauth")
	r.SetHostPort("web", 40300)
	r.SetHostPort("db", 40301)
	r.SetHostPort("redis", 40302)
	r.SetHostPort("storage", 40303)
	r.SetHostPort("mail", 40304)
	r.SetNamedHostPort("mail", "smtp", 40305)
	r.SetSecrets(func(string) (string, bool) { return "REDACTED", true })

	env, err := r.ResolveEnv(m.Services["web"].Env, PlaneContainer)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := env["NEXT_PRIVATE_INTERNAL_WEBAPP_URL"]; got != "http://web:3000" {
		t.Errorf("server-side URL = %q, want the container address http://web:3000", got)
	}
	if got := env["NEXT_PUBLIC_WEBAPP_URL"]; got != "http://add-oauth.documenso.localhost" {
		t.Errorf("browser URL = %q, want the bay origin", got)
	}
	if got := env["NEXT_PRIVATE_SMTP_PORT"]; got != "1025" {
		t.Errorf("named port on the container plane = %q, want the container port 1025", got)
	}
	if got := env["NEXT_PRIVATE_SMTP_HOST"]; got != "mail" {
		t.Errorf("smtp host = %q, want the service name", got)
	}
	if strings.Contains(env["NEXT_PRIVATE_DATABASE_URL"], "127.0.0.1") {
		t.Error("a container environment must not contain loopback addresses")
	}
}

func TestNamedPortsFollowThePlane(t *testing.T) {
	m := fixture(t, "documenso")
	r := NewResolver(m, "b1")
	r.SetNamedHostPort("mail", "smtp", 41025)

	if got, _ := r.ResolveString("${bay.mail.ports.smtp}", PlaneContainer); got != "1025" {
		t.Errorf("container plane = %q, want 1025", got)
	}
	got, err := r.ResolveString("${bay.mail.ports.smtp}", PlaneHost)
	if err != nil {
		t.Fatal(err)
	}
	if got != "41025" {
		t.Errorf("host plane = %q, want the published 41025", got)
	}
}

func TestSecretsResolveAtSpawnTime(t *testing.T) {
	m := fixture(t, "documenso")
	r := NewResolver(m, "b1")

	// With no source configured, a reference is an error rather than an empty
	// string: silently booting with a blank credential is worse than failing.
	if _, err := r.ResolveString("${secret:a/b}", PlaneContainer); err == nil {
		t.Error("unresolved secret should be an error")
	}

	var asked []string
	r.SetSecrets(func(p string) (string, bool) {
		asked = append(asked, p)
		return "sk_test_value", true
	})
	got, err := r.ResolveString("${secret:stripe/test}", PlaneContainer)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk_test_value" {
		t.Errorf("resolved secret = %q", got)
	}
	if len(asked) != 1 || asked[0] != "stripe/test" {
		t.Errorf("secret lookups = %v, want [stripe/test]", asked)
	}

	r.SetSecrets(func(string) (string, bool) { return "", false })
	if _, err := r.ResolveString("${secret:missing/one}", PlaneContainer); err == nil {
		t.Error("a missing secret should be an error")
	}
}

// Probes run from the host, so a service with no published port yet cannot be
// probed -- and must say so rather than silently probing the wrong thing.
func TestHostPlaneRequiresPublishedPort(t *testing.T) {
	m := fixture(t, "documenso")
	r := NewResolver(m, "b1")
	if _, err := r.Endpoint("db", PlaneHost); err == nil {
		t.Error("host endpoint before publishing should fail")
	}
}

func TestSchemeInference(t *testing.T) {
	for _, c := range []struct{ image, want string }{
		{"postgres:16", "postgres"},
		{"postgres:14@sha256:abc", "postgres"},
		{"pgvector/pgvector:pg16", "postgres"},
		{"mysql:8", "mysql"},
		{"mariadb:11", "mysql"},
		{"redis:7-alpine", "redis"},
		{"valkey/valkey:8", "redis"},
		{"mongo:7", "mongodb"},
		{"rabbitmq:3", "amqp"},
		{"node:22-bookworm", "http"},
		{"docker.elastic.co/elasticsearch/elasticsearch:8.19.15", "http"},
	} {
		if got := schemeFor(&manifest.Service{Image: c.image}); got != c.want {
			t.Errorf("schemeFor(%q) = %q, want %q", c.image, got, c.want)
		}
	}
}

// Every fixture must resolve end to end. This is the check that the format and
// the engine agree; a reference that validates but cannot be resolved would
// fail at boot instead of at validation, which is the wrong end.
func TestAllFixturesResolve(t *testing.T) {
	for _, name := range []string{"documenso", "mastodon", "saleor", "gitea", "fastapi-template"} {
		t.Run(name, func(t *testing.T) {
			m := fixture(t, name)
			r := NewResolver(m, "bay1")
			r.SetSecrets(func(string) (string, bool) { return "x", true })
			for svc, s := range m.Services {
				r.SetHostPort(svc, 40000)
				for pn := range s.Ports {
					r.SetNamedHostPort(svc, pn, 40001)
				}
			}
			for svc, s := range m.Services {
				if _, err := r.ResolveEnv(s.Env, PlaneContainer); err != nil {
					t.Errorf("%s: %v", svc, err)
				}
			}
		})
	}
}
