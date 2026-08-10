// Package egress enforces the per-service network allowlist.
//
// The rule is that a service reaches only what its manifest declares, and a
// service that declares nothing reaches nothing. That matters most for the
// step devbay runs on freshly cloned code it has never seen: installing
// dependencies. A malicious postinstall script is the delivery mechanism the
// self-replicating npm worms use, and its whole purpose is to reach the
// network with whatever credentials are lying around.
//
// # Why filtering, and not a network topology
//
// Docker has an `internal` network that blocks outbound traffic entirely, and
// it would be a much simpler mechanism. It cannot be used here: a container on
// an internal network cannot publish a port, and devbay publishes ports so the
// daemon can health-probe a service and an agent can call it. That was
// measured rather than assumed -- an internal-network container with `-p` fails
// to serve at all.
//
// So the filtering happens inside the container's own network namespace,
// applied by a short-lived privileged sidecar that joins that namespace and
// exits. This is deliberately scoped: the rules are written to one container's
// netns and never to the VM-wide chain, because a broad OUTPUT DROP applied at
// the VM level takes out the Docker VM's own DHCP and requires a factory reset
// to recover.
//
// # What this is and is not
//
// It reduces blast radius. It is not a containment boundary for hostile code,
// and must not be described as one. Allowlisting resolves hostnames to
// addresses, so a name that resolves to a shared CDN address permits every
// other name on that address; DNS answers change; and a determined process
// inside the container can still talk to anything sharing an allowed address.
package egress

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Image is the sidecar, built locally from baseImage.
//
// It has to ship iptables rather than install it at run time. The sidecar
// joins the target's network namespace, so once a policy is in force the
// sidecar inherits it -- and an install step inside that namespace cannot
// reach a package repository. Re-applying a policy would fail for the very
// reason the policy exists, which is a bootstrap problem rather than a
// configuration one.
//
// Building it locally also means the privileged container is the smallest
// thing that can do the job, rather than a general-purpose debugging image.
const Image = "devbay-egress:1"

const baseImage = "alpine:3.20"

const dockerfile = `FROM ` + baseImage + `
RUN apk add --no-cache iptables ip6tables
`

// DefaultAllowed is the policy a service inherits when it declares egress but
// names only what it specifically needs.
//
// Package registries are here because installing dependencies is the one thing
// almost every service must do and the one thing an allowlist most often
// forgets, producing a failure that looks like a broken lockfile rather than a
// blocked connection.
var DefaultAllowed = []string{
	"registry.npmjs.org",
	"registry.yarnpkg.com",
	"pypi.org",
	"files.pythonhosted.org",
	"rubygems.org",
	"index.rubygems.org",
	"proxy.golang.org",
	"sum.golang.org",
	"crates.io",
	"static.crates.io",
}

// Policy is the allowlist for one container.
type Policy struct {
	// Service names the service, for messages.
	Service string
	// Allow is the hostnames the service declared. Empty means no outbound.
	Allow []string
	// AllowDefaults adds the package registries in DefaultAllowed.
	AllowDefaults bool
}

// Enforcer applies policies to running containers.
type Enforcer struct {
	cli *client.Client
	Log func(format string, args ...any)

	// Resolve maps a hostname to addresses. Replaced in tests so the rules can
	// be checked without depending on the internet.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
}

// New returns an Enforcer.
func New(cli *client.Client, logf func(string, ...any)) *Enforcer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Enforcer{cli: cli, Log: logf, Resolve: resolveHost}
}

func resolveHost(ctx context.Context, host string) ([]net.IP, error) {
	var r net.Resolver
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if v4 := a.IP.To4(); v4 != nil {
			out = append(out, v4)
		}
	}
	return out, nil
}

// Apply installs the policy in a container's network namespace.
//
// It runs after the container has started, because a namespace has to exist
// before rules can be written to it. The window between start and enforcement
// is real and is the reason install steps run as their own container: an
// install is enforced before the long-running service is even created.
func (e *Enforcer) Apply(ctx context.Context, containerID string, p Policy) error {
	if err := e.ensureImage(ctx); err != nil {
		return err
	}

	allow := append([]string{}, p.Allow...)
	if p.AllowDefaults {
		allow = append(allow, DefaultAllowed...)
	}

	var addrs []string
	var unresolved []string
	for _, host := range dedupe(allow) {
		ips, err := e.Resolve(ctx, host)
		if err != nil || len(ips) == 0 {
			// A name that will not resolve now is recorded rather than
			// silently dropped: the service is about to fail to reach it, and
			// the reason should already be on screen.
			unresolved = append(unresolved, host)
			continue
		}
		for _, ip := range ips {
			addrs = append(addrs, ip.String())
		}
	}
	if len(unresolved) > 0 {
		e.Log("  egress: %s: could not resolve %s; traffic to those names will be blocked",
			p.Service, strings.Join(unresolved, ", "))
	}

	locals, err := e.attachedNetworks(ctx, containerID)
	if err != nil {
		return fmt.Errorf("egress: reading the networks of %s: %w", p.Service, err)
	}
	script := rules(locals, dedupe(addrs))
	out, code, err := e.runSidecar(ctx, containerID, script)
	if err != nil {
		return fmt.Errorf("egress: applying policy to %s: %w", p.Service, err)
	}
	if code != 0 {
		return fmt.Errorf("egress: applying policy to %s failed (%d):\n%s", p.Service, code, out)
	}

	switch {
	case len(allow) == 0:
		e.Log("  egress: %s has no outbound network", p.Service)
	default:
		e.Log("  egress: %s may reach %d host(s)", p.Service, len(dedupe(allow)))
	}
	return nil
}

// local describes one network a container is attached to.
type local struct {
	Subnet  string
	Gateway string
}

// rules renders the iptables program applied inside the namespace.
//
// The order is the policy, and two decisions in it are load-bearing.
//
// Only the container's OWN subnets are permitted, not the private address
// space in general. Allowing RFC1918 wholesale looks harmless and is not:
// Docker's bridges live in 172.16/12, so it would silently permit every other
// bay on the machine, the developer's LAN, and anything else on a private
// address -- an allowlist with a hole the size of the local network.
//
// The gateway is then rejected explicitly, ahead of the subnet it belongs to.
// Peer containers are reached directly and stay reachable, but the gateway is
// the route to everything else, including whatever the host has published. A
// service has no business reaching either.
func rules(locals []local, addrs []string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("iptables -F OUTPUT || true\n")
	b.WriteString("iptables -A OUTPUT -o lo -j ACCEPT\n")
	b.WriteString("iptables -A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT\n")
	// Docker's embedded resolver lives on loopback, and a name has to resolve
	// before an allowed address can be reached.
	b.WriteString("iptables -A OUTPUT -d 127.0.0.0/8 -j ACCEPT\n")
	b.WriteString("iptables -A OUTPUT -p udp --dport 53 -j ACCEPT\n")
	b.WriteString("iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT\n")

	for _, l := range locals {
		if l.Gateway != "" {
			fmt.Fprintf(&b, "iptables -A OUTPUT -d %s -j REJECT --reject-with icmp-admin-prohibited\n", l.Gateway)
		}
	}
	for _, l := range locals {
		if l.Subnet != "" {
			fmt.Fprintf(&b, "iptables -A OUTPUT -d %s -j ACCEPT\n", l.Subnet)
		}
	}
	for _, a := range addrs {
		fmt.Fprintf(&b, "iptables -A OUTPUT -d %s -j ACCEPT\n", a)
	}
	// Rejected rather than dropped, so a blocked call fails at once instead of
	// hanging until some library's timeout.
	b.WriteString("iptables -A OUTPUT -j REJECT --reject-with icmp-admin-prohibited\n")
	return b.String()
}

// attachedNetworks reports the subnets and gateways a container can see.
func (e *Enforcer) attachedNetworks(ctx context.Context, containerID string) ([]local, error) {
	ins, err := e.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	var out []local
	for _, ep := range ins.Container.NetworkSettings.Networks {
		var l local
		if ep.Gateway.IsValid() {
			l.Gateway = ep.Gateway.String()
		}
		if ep.IPAddress.IsValid() && ep.IPPrefixLen > 0 {
			// The subnet is derived from the container's own address and mask,
			// which is what it can actually reach directly.
			ip := net.ParseIP(ep.IPAddress.String()).To4()
			if ip != nil {
				mask := net.CIDRMask(ep.IPPrefixLen, 32)
				l.Subnet = (&net.IPNet{IP: ip.Mask(mask), Mask: mask}).String()
			}
		}
		if l.Subnet != "" || l.Gateway != "" {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subnet < out[j].Subnet })
	return out, nil
}

// runSidecar executes a script in the target container's network namespace.
//
// `--network container:<id>` is what confines this: the sidecar joins one
// container's namespace, writes rules that apply only there, and exits. It
// never sees the VM's own chains, which is the difference between filtering a
// service and disabling the machine's networking.
func (e *Enforcer) runSidecar(ctx context.Context, targetID, script string) (string, int, error) {
	res, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: Image,
			// The script is generated here from validated data, never from a
			// manifest string: the manifest cannot express a shell command.
			Cmd:    []string{"sh", "-c", script},
			Labels: map[string]string{"dev.devbay.egress": "1"},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode("container:" + targetID),
			CapAdd:      []string{"NET_ADMIN", "NET_RAW"},
			AutoRemove:  false,
		},
	})
	if err != nil {
		return "", 0, err
	}
	defer func() {
		_, _ = e.cli.ContainerRemove(context.WithoutCancel(ctx), res.ID,
			client.ContainerRemoveOptions{Force: true})
	}()

	if _, err := e.cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		return "", 0, err
	}

	wait := e.cli.ContainerWait(ctx, res.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	var code int
	select {
	case err := <-wait.Error:
		return "", 0, err
	case st := <-wait.Result:
		code = int(st.StatusCode)
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}

	logs, _ := e.cli.ContainerLogs(ctx, res.ID, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: "40",
	})
	var out strings.Builder
	if logs != nil {
		defer logs.Close()
		buf := make([]byte, 8<<10)
		for {
			n, err := logs.Read(buf)
			out.Write(buf[:n])
			if err != nil || out.Len() > 8<<10 {
				break
			}
		}
	}
	return out.String(), code, nil
}

// ensureImage builds the sidecar image if it is not already present.
//
// The build happens before any policy is applied, in a normal namespace with
// working networking. Doing it lazily inside the restricted namespace is what
// does not work.
func (e *Enforcer) ensureImage(ctx context.Context) error {
	found, err := e.cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("reference", Image),
	})
	if err == nil && len(found.Items) > 0 {
		return nil
	}

	tar, err := buildContext()
	if err != nil {
		return err
	}
	e.Log("  egress: building %s", Image)
	res, err := e.cli.ImageBuild(ctx, tar, client.ImageBuildOptions{
		Tags:       []string{Image},
		Dockerfile: "Dockerfile",
		Remove:     true,
	})
	if err != nil {
		return fmt.Errorf("egress: building %s: %w", Image, err)
	}
	defer res.Body.Close()
	// The build only runs when the response body is consumed.
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		return fmt.Errorf("egress: building %s: %w", Image, err)
	}

	found, err = e.cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("reference", Image),
	})
	if err != nil || len(found.Items) == 0 {
		return fmt.Errorf("egress: %s was not produced by the build", Image)
	}
	return nil
}

// buildContext returns a tar stream containing just the Dockerfile.
func buildContext() (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "Dockerfile",
		Mode: 0o644,
		Size: int64(len(dockerfile)),
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write([]byte(dockerfile)); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// Cleanup removes any sidecar left behind by an interrupted run.
func (e *Enforcer) Cleanup(ctx context.Context) error {
	list, err := e.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", "dev.devbay.egress=1"),
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range list.Items {
		if _, err := e.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
