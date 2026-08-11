package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/testutil"
)

func dockerOrSkip(t *testing.T) *client.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Docker integration test in short mode")
	}
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no Docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("Docker daemon is not responding: %v", err)
	}
	return cli
}

// The routing table is rendered without any Docker involvement, so this checks
// the part that decides whether a request reaches the right bay.
func TestCaddyConfigRendersRoutes(t *testing.T) {
	p := New(nil, nil)
	p.routes = map[string]Route{
		"b.acme.localhost":     {Host: "b.acme.localhost", Upstream: "web:3000"},
		"api.b.acme.localhost": {Host: "api.b.acme.localhost", Upstream: "api:8000"},
	}

	blob, err := json.Marshal(p.caddyConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(blob)
	for _, want := range []string{"b.acme.localhost", "api.b.acme.localhost", "web:3000", "api:8000", "reverse_proxy"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config is missing %q:\n%s", want, cfg)
		}
	}
	// An app that builds absolute URLs from the upstream address would
	// redirect the browser off the bay origin and undo the isolation.
	if !strings.Contains(cfg, "X-Forwarded-Host") {
		t.Error("config must forward the original host")
	}
}

// Routes are replaced per bay, not merged: a service dropped from a manifest
// must stop being routable rather than linger as a hostname pointing at a
// container that no longer exists.
func TestSetRoutesReplacesOnlyThatBay(t *testing.T) {
	p := New(nil, nil)
	ctx := context.Background()

	if err := p.SetRoutes(ctx, "acme", "one", []Route{
		{Host: "one.acme.localhost", Upstream: "web:3000"},
		{Host: "api.one.acme.localhost", Upstream: "api:8000"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetRoutes(ctx, "acme", "two", []Route{
		{Host: "two.acme.localhost", Upstream: "web:3000"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := len(p.Routes()); got != 3 {
		t.Fatalf("got %d routes, want 3", got)
	}

	// Re-declaring bay one with fewer services drops the extra route.
	if err := p.SetRoutes(ctx, "acme", "one", []Route{
		{Host: "one.acme.localhost", Upstream: "web:3000"},
	}); err != nil {
		t.Fatal(err)
	}
	hosts := map[string]bool{}
	for _, r := range p.Routes() {
		hosts[r.Host] = true
	}
	if hosts["api.one.acme.localhost"] {
		t.Error("a route removed from the manifest is still live")
	}
	if !hosts["two.acme.localhost"] {
		t.Error("replacing bay one disturbed bay two")
	}

	if err := p.ClearRoutes(ctx, "acme", "one"); err != nil {
		t.Fatal(err)
	}
	for _, r := range p.Routes() {
		if r.Bay == "one" {
			t.Errorf("route %s survived ClearRoutes", r.Host)
		}
	}
}

// ---------------------------------------------------------------------------
// The test this package exists for.
//
// Two bays running the same application must keep independent login sessions.
// Browsers key cookies by host and ignore the port, so serving both from
// localhost on different ports puts them in one storage partition: logging
// into one logs you into the other. That is the bug per-bay origins eliminate,
// and it is worth proving rather than asserting.
// ---------------------------------------------------------------------------

func TestTwoBaysKeepSeparateCookieJars(t *testing.T) {
	cli := dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	p := New(cli, func(f string, a ...any) { t.Logf(f, a...) })
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = p.Stop(c)
	})

	// A high port: binding :80 on a developer's machine during a test would be
	// rude, and the isolation being tested does not depend on the number.
	if err := p.Ensure(ctx, 18080, 12019); err != nil {
		t.Fatalf("starting proxy: %v", err)
	}

	// Two "bays": each is a network plus a server that sets a session cookie
	// scoped to whatever host it was asked for.
	type bay struct{ name, netName, container string }
	bays := []bay{
		{name: "alpha", netName: "devbaytest-proxy-alpha"},
		{name: "beta", netName: "devbaytest-proxy-beta"},
	}

	for i := range bays {
		b := &bays[i]
		id := startEchoServer(t, cli, ctx, b.netName, b.name)
		b.container = id
		if err := p.Attach(ctx, b.netName); err != nil {
			t.Fatalf("attaching to %s: %v", b.netName, err)
		}
		if err := p.SetRoutes(ctx, "acme", b.name, []Route{{
			Host:     b.name + ".acme.localhost",
			Upstream: "app:80",
		}}); err != nil {
			t.Fatalf("routing %s: %v", b.name, err)
		}
	}

	// Wait for each bay to answer before asserting anything. devbay's engine
	// health-probes before declaring a bay ready; a test that races the
	// application's startup would fail for a reason unrelated to isolation.
	for _, b := range bays {
		waitRoute(t, ctx, p.HTTPPort, b.name+".acme.localhost")
	}

	// One browser, one cookie jar, both bays — which is precisely the
	// situation that breaks without distinct origins.
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{Jar: jar, Timeout: 15 * time.Second}

	get := func(host, path string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("http://127.0.0.1:%d%s", p.HTTPPort, path), nil)
		if err != nil {
			t.Fatal(err)
		}
		// Requests are addressed to the proxy by IP with an explicit Host
		// header, because Go's resolver does not special-case *.localhost —
		// only browsers and curl do. The proxy routes on the header, which is
		// what a browser would send.
		req.Host = host
		req.URL.Host = fmt.Sprintf("127.0.0.1:%d", p.HTTPPort)

		resp, err := browser.Do(req)
		if err != nil {
			t.Fatalf("GET %s%s: %v", host, path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s%s = %s: %s", host, path, resp.Status, body)
		}
		return strings.TrimSpace(string(body))
	}

	// Log in to alpha only.
	get("alpha.acme.localhost", "/login?user=alice")

	if got := get("alpha.acme.localhost", "/whoami"); got != "alice" {
		t.Errorf("alpha should be logged in as alice, got %q", got)
	}
	// The assertion that matters. Without per-bay origins this returns alice,
	// because both bays shared one cookie jar.
	if got := get("beta.acme.localhost", "/whoami"); got != "anonymous" {
		t.Errorf("beta leaked alpha's session: /whoami returned %q, want anonymous", got)
	}

	// And the reverse: a second login must not disturb the first.
	get("beta.acme.localhost", "/login?user=bob")
	if got := get("beta.acme.localhost", "/whoami"); got != "bob" {
		t.Errorf("beta should be logged in as bob, got %q", got)
	}
	if got := get("alpha.acme.localhost", "/whoami"); got != "alice" {
		t.Errorf("logging into beta overwrote alpha's session: got %q, want alice", got)
	}

	// The jar itself should show two independent cookies, which is the
	// underlying mechanism rather than its symptom.
	t.Logf("cookie jar holds sessions for %d distinct origins", countHosts(jar))

	// Detaching must leave each bay's network removable.
	for _, b := range bays {
		if err := p.Detach(ctx, b.netName); err != nil {
			t.Errorf("detach %s: %v", b.netName, err)
		}
		_, _ = cli.ContainerRemove(ctx, b.container, client.ContainerRemoveOptions{Force: true})
		if _, err := cli.NetworkRemove(ctx, b.netName, client.NetworkRemoveOptions{}); err != nil {
			t.Errorf("network %s could not be removed after detach: %v", b.netName, err)
		}
	}
}

// waitRoute polls a bay's hostname through the proxy until it answers.
func waitRoute(t *testing.T, ctx context.Context, port int, host string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("http://127.0.0.1:%d/whoami", port), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
			last = fmt.Sprintf("%s: %s", resp.Status, body)
		} else {
			last = err.Error()
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("%s never answered: %v", host, ctx.Err())
		}
	}
	t.Fatalf("%s never answered through the proxy; last response: %s", host, last)
}

// startEchoServer runs a tiny session server on its own network, aliased "app".
// appImage runs the session server the isolation test talks to.
const appImage = "python:3.12-alpine"

func startEchoServer(t *testing.T, cli *client.Client, ctx context.Context, netName, label string) string {
	t.Helper()

	if _, err := cli.NetworkCreate(ctx, netName, client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: map[string]string{"dev.devbay.test": "1"},
	}); err != nil && !strings.Contains(err.Error(), "exists") {
		t.Fatalf("creating network %s: %v", netName, err)
	}
	// Confirmed rather than assumed. A create that reported "already exists"
	// against a network that has since been removed leaves this pointing at
	// nothing, and the symptom surfaces later and elsewhere -- as a container
	// that will not start, naming a network the test believes it just made.
	if _, err := cli.NetworkInspect(ctx, netName, client.NetworkInspectOptions{}); err != nil {
		t.Fatalf("network %s does not exist after creating it: %v", netName, err)
	}

	// Pulled explicitly: this test creates containers through the Docker API
	// rather than through the engine, so nothing else fetches the image.
	testutil.PullImage(ctx, t, cli, appImage)

	// A session server in one line of Python: /login sets a cookie, /whoami
	// reads it back. The cookie has no Domain attribute, so the browser scopes
	// it to the exact host it was served from — which is the behaviour under
	// test.
	const script = `
import http.server, urllib.parse
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        q = urllib.parse.parse_qs(u.query)
        if u.path == "/login":
            user = q.get("user", ["anonymous"])[0]
            self.send_response(200)
            self.send_header("Set-Cookie", "session=%s; Path=/" % user)
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(b"ok")
            return
        cookie = self.headers.get("Cookie") or ""
        user = "anonymous"
        for part in cookie.split(";"):
            k, _, v = part.strip().partition("=")
            if k == "session":
                user = v
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(user.encode())
    def log_message(self, *a): pass
http.server.HTTPServer(("0.0.0.0", 80), H).serve_forever()
`
	res, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "devbaytest-proxy-app-" + label,
		Config: &container.Config{
			Image:  appImage,
			Cmd:    []string{"python3", "-c", script},
			Labels: map[string]string{"dev.devbay.test": "1"},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				netName: {Aliases: []string{"app"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("creating app container: %v", err)
	}
	if _, err := cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("starting app container: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = cli.ContainerRemove(c, res.ID, client.ContainerRemoveOptions{Force: true})
		_, _ = cli.NetworkRemove(c, netName, client.NetworkRemoveOptions{})
	})
	return res.ID
}

func countHosts(jar *cookiejar.Jar) int {
	n := 0
	for _, host := range []string{"alpha.acme.localhost", "beta.acme.localhost"} {
		u := &url.URL{Scheme: "http", Host: host, Path: "/"}
		if len(jar.Cookies(u)) > 0 {
			n++
		}
	}
	return n
}
