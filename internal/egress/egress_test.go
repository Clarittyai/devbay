package egress

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/testutil"
)

// The rule program is generated, so its shape can be checked without Docker.
// Order is the policy here, and a rule in the wrong place is a silent hole.
func TestRuleOrderIsThePolicy(t *testing.T) {
	script := rules([]local{{Subnet: "172.20.0.0/16", Gateway: "172.20.0.1"}}, []string{"93.184.216.34"})
	lines := strings.Split(strings.TrimSpace(script), "\n")

	index := func(needle string) int {
		for i, l := range lines {
			if strings.Contains(l, needle) {
				return i
			}
		}
		return -1
	}

	// The catch-all is the REJECT with no destination. Searching for "REJECT"
	// alone would now find the gateway rule, which is a targeted rejection
	// rather than the default.
	reject := -1
	for i, l := range lines {
		if strings.Contains(l, "-j REJECT") && !strings.Contains(l, "-d ") {
			reject = i
		}
	}
	if reject < 0 {
		t.Fatal("no default rejection; the allowlist would permit everything")
	}
	// Anything that must be permitted has to precede the rejection, or it is
	// not permitted at all.
	for _, before := range []string{
		"-o lo -j ACCEPT",
		"ESTABLISHED,RELATED",
		"--dport 53",
		"172.20.0.0/16",
		"93.184.216.34",
	} {
		i := index(before)
		if i < 0 {
			t.Errorf("missing rule %q", before)
			continue
		}
		if i > reject {
			t.Errorf("rule %q comes after the default rejection, so it never applies", before)
		}
	}
	// The rejection must be last.
	if reject != len(lines)-1 {
		t.Errorf("the rejection is at line %d of %d; it must be last", reject+1, len(lines))
	}
	// Rejecting rather than dropping means a blocked call fails immediately
	// instead of hanging until some library's timeout.
	if !strings.Contains(lines[reject], "icmp-admin-prohibited") {
		t.Error("blocked traffic should be rejected, not black-holed")
	}
}

// A service that declares nothing, on no network, reaches nothing.
func TestEmptyPolicyProducesNoAllowances(t *testing.T) {
	script := rules(nil, nil)
	if strings.Contains(script, "-d 10.0.0.0/8") || strings.Contains(script, "-d 172.16.0.0/12") {
		t.Errorf("private space is allowed wholesale, which permits every other bay:\n%s", script)
	}
	if !strings.Contains(script, "REJECT") {
		t.Error("an empty policy must still reject")
	}
}

// The container's own subnet is reachable so services in a bay can talk to
// each other. Private space in general is not: Docker's bridges live in
// 172.16/12, so allowing it wholesale would permit every other bay on the
// machine and the developer's own network.
func TestOnlyOwnSubnetIsAllowed(t *testing.T) {
	script := rules([]local{{Subnet: "172.20.0.0/16", Gateway: "172.20.0.1"}}, nil)

	if !strings.Contains(script, "-d 172.20.0.0/16 -j ACCEPT") {
		t.Error("the container's own subnet must stay reachable")
	}
	for _, wide := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if strings.Contains(script, "-d "+wide) {
			t.Errorf("%s is allowed wholesale; that is a hole the size of the local network", wide)
		}
	}
}

// The gateway is the route to everything else, including whatever the host has
// published, so it is rejected ahead of the subnet it belongs to.
func TestGatewayIsRejectedBeforeItsSubnet(t *testing.T) {
	script := rules([]local{{Subnet: "172.20.0.0/16", Gateway: "172.20.0.1"}}, nil)
	lines := strings.Split(script, "\n")

	gw, subnet := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "-d 172.20.0.1 ") && strings.Contains(l, "REJECT") {
			gw = i
		}
		if strings.Contains(l, "-d 172.20.0.0/16 -j ACCEPT") {
			subnet = i
		}
	}
	if gw < 0 {
		t.Fatal("the gateway is not rejected; the host and the internet stay reachable through it")
	}
	if subnet < 0 {
		t.Fatal("the subnet is not allowed")
	}
	if gw > subnet {
		t.Error("the gateway rejection comes after the subnet allowance, so it never applies")
	}
}

func TestDedupe(t *testing.T) {
	got := dedupe([]string{"b", "a", "b", "", "a", "c"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("dedupe = %v, want sorted and unique", got)
	}
}

// ---------------------------------------------------------------------------
// Against real containers.
//
// The "outside" is a container on its own network rather than the internet, so
// the test is deterministic, works offline, and does not depend on some remote
// host staying up.
// ---------------------------------------------------------------------------

func dockerOrSkip(t *testing.T) *client.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("needs Docker")
	}
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no Docker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("Docker not responding: %v", err)
	}
	return cli
}

// The "outside" is a real address on the internet, because that is what the
// policy claims to block and anything closer is a weaker claim.
//
// A peer container will not do: a peer shares the subnet and is meant to stay
// reachable. An earlier version of this test used one and passed while the
// policy had a hole the size of the local network -- every other bay, the
// developer's LAN, and the host all reachable. A server on the host will not
// do either: on Docker Desktop the gateway lives inside the VM and does not
// route to it.
const (
	outsideAddr = "1.1.1.1"
	outsidePort = 443
)

// bay creates a network for the subject and returns its name.
func bayNetwork(t *testing.T, cli *client.Client, ctx context.Context, name string) string {
	t.Helper()
	netName := "devbay-egress-test-" + name
	_, _ = cli.NetworkRemove(ctx, netName, client.NetworkRemoveOptions{})
	if _, err := cli.NetworkCreate(ctx, netName, client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: map[string]string{testLabel: "1"},
	}); err != nil {
		t.Fatalf("creating %s: %v", netName, err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = cli.NetworkRemove(c, netName, client.NetworkRemoveOptions{})
	})
	return netName
}

// subjectImage is the container these tests apply a policy to. Pinned to a
// minor version so a moving tag cannot change what "no egress" is being proved
// about.
const subjectImage = "alpine:3.20"

// subject starts a container on a network and returns its id and gateway.
func subject(t *testing.T, cli *client.Client, ctx context.Context, name, netName string) (id, gateway, ip string) {
	t.Helper()
	// Pulled explicitly: this test reaches for the Docker API directly rather
	// than going through the engine, so nothing else will have fetched it, and
	// on a machine that has run these tests before it is already cached --
	// which is why the omission survived until CI ran on a fresh runner.
	testutil.PullImage(ctx, t, cli, subjectImage)
	res, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "devbay-egress-test-" + name,
		Config: &container.Config{
			Image:  subjectImage,
			Cmd:    []string{"sleep", "300"},
			Labels: map[string]string{testLabel: "1"},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{netName: {}},
		},
	})
	if err != nil {
		t.Fatalf("creating the subject: %v", err)
	}
	if _, err := cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("starting the subject: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = cli.ContainerRemove(c, res.ID, client.ContainerRemoveOptions{Force: true})
	})

	ins, err := cli.ContainerInspect(ctx, res.ID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range ins.Container.NetworkSettings.Networks {
		if ep.Gateway.IsValid() {
			gateway = ep.Gateway.String()
		}
		if ep.IPAddress.IsValid() {
			ip = ep.IPAddress.String()
		}
	}
	if gateway == "" {
		t.Fatal("the subject has no gateway, so there is nothing to test against")
	}
	return res.ID, gateway, ip
}

// canReach reports whether the subject can open a TCP connection.
func canReach(t *testing.T, cli *client.Client, ctx context.Context, id, addr string, port int) bool {
	t.Helper()
	ex, err := cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		Cmd:          []string{"timeout", "4", "nc", "-z", "-w", "3", addr, fmt.Sprint(port)},
		AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		t.Fatalf("exec create: %v", err)
	}
	att, err := cli.ExecAttach(ctx, ex.ID, client.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("exec attach: %v", err)
	}
	att.Close()

	for i := 0; i < 100; i++ {
		ins, err := cli.ExecInspect(ctx, ex.ID, client.ExecInspectOptions{})
		if err != nil {
			t.Fatalf("exec inspect: %v", err)
		}
		if !ins.Running {
			return ins.ExitCode == 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the reachability probe never finished")
	return false
}

// The claim the package exists to make: a service that declares no egress
// cannot reach anything outside its bay.
func TestNoEgressMeansNoOutbound(t *testing.T) {
	cli := dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	netName := bayNetwork(t, cli, ctx, "closed-net")
	id, _, _ := subject(t, cli, ctx, "closed", netName)

	// Before enforcement it must be reachable, or the test proves nothing.
	if !canReach(t, cli, ctx, id, outsideAddr, outsidePort) {
		t.Skip("no internet access from a container here")
	}

	e := New(cli, func(f string, a ...any) { t.Logf(f, a...) })
	if err := e.Apply(ctx, id, Policy{Service: "closed"}); err != nil {
		t.Fatalf("applying an empty policy: %v", err)
	}

	if canReach(t, cli, ctx, id, outsideAddr, outsidePort) {
		t.Error("a service with no declared egress still reached the internet")
	}
}

// Services in the same bay must keep talking to each other; that is the
// difference between a policy and an off switch.
func TestPeersInTheSameBayStayReachable(t *testing.T) {
	cli := dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	netName := bayNetwork(t, cli, ctx, "peers-net")
	id, _, _ := subject(t, cli, ctx, "peer-a", netName)
	peerID, _, peerIP := subject(t, cli, ctx, "peer-b", netName)
	_ = peerID

	// Give the peer something to answer on.
	ex, err := cli.ExecCreate(ctx, peerID, client.ExecCreateOptions{
		Cmd: []string{"sh", "-c", "nc -l -p 8080 -k >/dev/null 2>&1 &"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.ExecStart(ctx, ex.ID, client.ExecStartOptions{Detach: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	e := New(cli, func(f string, a ...any) { t.Logf(f, a...) })
	if err := e.Apply(ctx, id, Policy{Service: "peer-a"}); err != nil {
		t.Fatalf("applying: %v", err)
	}

	if !canReach(t, cli, ctx, id, peerIP, 8080) {
		t.Error("a peer in the same bay became unreachable; the bay cannot function")
	}
}

// A declared host is reachable, which is what makes this an allowlist rather
// than an off switch.
func TestDeclaredHostIsReachable(t *testing.T) {
	cli := dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	netName := bayNetwork(t, cli, ctx, "allowed-net")
	id, _, _ := subject(t, cli, ctx, "allowed", netName)

	if !canReach(t, cli, ctx, id, outsideAddr, outsidePort) {
		t.Skip("no internet access from a container here")
	}

	e := New(cli, func(f string, a ...any) { t.Logf(f, a...) })
	// Resolution is stubbed so the test pins one address rather than depending
	// on whatever DNS returns today.
	e.Resolve = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "allowed.example" {
			return []net.IP{net.ParseIP(outsideAddr).To4()}, nil
		}
		return nil, net.UnknownNetworkError(host)
	}

	if err := e.Apply(ctx, id, Policy{Service: "allowed", Allow: []string{"allowed.example"}}); err != nil {
		t.Fatalf("applying: %v", err)
	}
	if !canReach(t, cli, ctx, id, outsideAddr, outsidePort) {
		t.Error("a declared host was blocked")
	}
	// And an undeclared address on the same internet is not.
	if canReach(t, cli, ctx, id, "8.8.8.8", 443) {
		t.Error("an undeclared address was reachable; this is an allowlist, not an off switch")
	}
}

// A name that cannot be resolved is reported rather than silently dropped.
func TestUnresolvableHostIsReported(t *testing.T) {
	cli := dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	netName := bayNetwork(t, cli, ctx, "unresolved-net")
	id, _, _ := subject(t, cli, ctx, "unresolved", netName)

	var logged strings.Builder
	e := New(cli, func(f string, a ...any) { logged.WriteString(strings.TrimSpace(f)) })
	e.Resolve = func(context.Context, string) ([]net.IP, error) {
		return nil, net.UnknownNetworkError("nope")
	}

	if err := e.Apply(ctx, id, Policy{Service: "x", Allow: []string{"nowhere.invalid"}}); err != nil {
		t.Fatalf("applying: %v", err)
	}
	if !strings.Contains(logged.String(), "could not resolve") {
		t.Errorf("an unresolvable host should be reported, got: %s", logged.String())
	}
}

// Enforcement must be repeatable: the rules are flushed and rewritten rather
// than appended, or a second application would stack duplicate allowances.
func TestApplyingTwiceIsStable(t *testing.T) {
	cli := dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	netName := bayNetwork(t, cli, ctx, "twice-net")
	id, _, _ := subject(t, cli, ctx, "twice", netName)

	if !canReach(t, cli, ctx, id, outsideAddr, outsidePort) {
		t.Skip("no internet access from a container here")
	}

	e := New(cli, func(string, ...any) {})
	for i := 0; i < 2; i++ {
		if err := e.Apply(ctx, id, Policy{Service: "twice"}); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	if canReach(t, cli, ctx, id, outsideAddr, outsidePort) {
		t.Error("outbound was reachable after applying a closed policy twice")
	}
}

// testLabel marks what this package created, and is deliberately unique to it.
//
// The sweep below runs machine-wide, and `go test ./...` runs packages
// concurrently: a label shared with another package's tests means this one
// deletes that one's containers and networks out from under it, mid-run. That
// is exactly what happened -- the proxy package used the same generic
// `dev.devbay.test`, and its bays lost their networks partway through a test
// whose own logic was fine. The symptom appeared far from the cause, in a
// different package, and only on a machine slow enough for the runs to
// overlap.
const testLabel = "dev.devbay.test.egress"

func TestMain(m *testing.M) {
	code := m.Run()
	if cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation()); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)

		f := make(client.Filters).Add("label", testLabel+"=1")
		if cs, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f}); err == nil {
			for _, c := range cs.Items {
				_, _ = cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
			}
		}
		if ns, err := cli.NetworkList(ctx, client.NetworkListOptions{Filters: f}); err == nil {
			for _, n := range ns.Items {
				_, _ = cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
			}
		}

		// Sidecars are devbay's own and carry a product label, so they cannot
		// be marked as belonging to these tests. Only finished ones are swept:
		// a running sidecar is applying a policy for a test somewhere else,
		// and removing it would make that test fail for no visible reason.
		sf := make(client.Filters).Add("label", "dev.devbay.egress=1")
		if cs, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: sf}); err == nil {
			for _, c := range cs.Items {
				if c.State == "running" || c.State == "created" {
					continue
				}
				_, _ = cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
			}
		}

		cancel()
		cli.Close()
	}
	os.Exit(code)
}

// A policy that fails halfway must fail closed.
//
// This is the failure that matters most and shows least: the sidecar dies
// after flushing and before the rules are in, and the container is left with
// an empty chain -- which, under the default ACCEPT policy, means the whole
// internet. Nothing downstream notices, because a service with no declared
// egress is expected to be silent about the network either way.
func TestAPartialApplicationFailsClosed(t *testing.T) {
	cli := dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	netName := bayNetwork(t, cli, ctx, "partial-net")
	id, _, _ := subject(t, cli, ctx, "partial", netName)

	if !canReach(t, cli, ctx, id, outsideAddr, outsidePort) {
		t.Skip("no internet access from a container here")
	}

	e := New(cli, func(string, ...any) {})
	if err := e.ensureImage(ctx); err != nil {
		t.Fatal(err)
	}

	// The real script, truncated after the flush -- exactly what a crash, an
	// xtables lock contention or an OOM would leave behind.
	broken := "set -e\niptables -P OUTPUT DROP\niptables -F OUTPUT || true\nexit 1\n"
	if _, code, err := e.runSidecar(ctx, id, broken); err != nil {
		t.Fatalf("running the truncated script: %v", err)
	} else if code == 0 {
		t.Fatal("a script that exited 1 was reported as successful, so a real failure would be silent")
	}

	if canReach(t, cli, ctx, id, outsideAddr, outsidePort) {
		t.Error("a half-applied policy left the container with unrestricted outbound access")
	}
}
