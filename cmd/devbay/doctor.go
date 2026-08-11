package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/ports"
	"github.com/Clarittyai/devbay/internal/proxy"
)

// stdio is os.Stdin for reading and os.Stdout for writing.
//
// Needed because the MCP transport is one bidirectional byte stream, and the
// two halves of that stream are different files.
type stdio struct {
	in  io.Reader
	out io.Writer
}

func (s stdio) Read(p []byte) (int, error)  { return s.in.Read(p) }
func (s stdio) Write(p []byte) (int, error) { return s.out.Write(p) }

// cmdDoctor reports whether this machine can run devbay well.
//
// It leads with the things that are quietly wrong rather than the things that
// are loudly broken: a stopped Docker daemon announces itself, but a VM that
// never returns memory to the host just makes the machine feel slow an hour
// later, and nothing connects that to devbay.
func cmdDoctor(ctx context.Context) error {
	var problems int
	check := func(ok bool, label, detail string) {
		switch {
		case ok:
			fmt.Printf("  %s %s\n", green("ok"), label)
		default:
			problems++
			fmt.Printf("  %s %s\n", red("!!"), label)
		}
		if detail != "" {
			fmt.Printf("       %s\n", dim(detail))
		}
	}
	note := func(label, detail string) {
		fmt.Printf("  %s %s\n", yellow("--"), label)
		if detail != "" {
			fmt.Printf("       %s\n", dim(detail))
		}
	}

	fmt.Println("git")
	if out, err := exec.Command("git", "--version").Output(); err != nil {
		check(false, "git is not on PATH", "devbay manages worktrees, so git is required")
	} else {
		check(true, strings.TrimSpace(string(out)), "")
	}

	fmt.Println("\ndocker")
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		check(false, "cannot create a Docker client", err.Error())
		return summarise(problems)
	}
	defer cli.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(pingCtx, client.PingOptions{}); err != nil {
		check(false, "the Docker daemon is not responding", "start Docker Desktop, OrbStack, or colima and try again")
		return summarise(problems)
	}

	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		check(true, "daemon is responding", "could not read system info: "+err.Error())
	} else {
		i := info.Info
		check(true, fmt.Sprintf("daemon %s (%s)", i.ServerVersion, i.OperatingSystem), "")

		gb := float64(i.MemTotal) / (1 << 30)
		switch {
		case gb < 4:
			check(false, fmt.Sprintf("VM memory %.1f GiB", gb),
				"five bays will not fit; raise the memory limit in your Docker runtime's settings")
		case gb > 24:
			// The default on a large Mac is close to all of host RAM, which is
			// exactly the configuration that ratchets and never gives back.
			note(fmt.Sprintf("VM memory %.1f GiB", gb),
				"that is most of this machine. Capping it to 12-16 GiB leaves headroom for macOS, "+
					"and matters more than usual because of the reclamation issue below")
		default:
			check(true, fmt.Sprintf("VM memory %.1f GiB", gb), "")
		}
		check(i.NCPU >= 4, fmt.Sprintf("VM CPUs %d", i.NCPU), "")

		checkDisk(ctx, cli, check, note)

		// The finding that most affects how devbay should be used on a Mac.
		if isAppleVZ(i.OperatingSystem, i.KernelVersion, i.Name) {
			note("memory reclamation on this runtime is limited",
				"Under Apple's Virtualization.framework the Linux VM does not return freed memory to macOS, "+
					"so a bay you stop frees memory inside the VM but the VM keeps it. "+
					"OrbStack, or Docker Desktop with its own VMM enabled, do return it.")
		}
	}

	fmt.Println("\ndevbay")
	if _, err := ports.Open(""); err != nil {
		check(false, "cannot open the state database", err.Error())
	} else {
		check(true, "state database is writable", "")
	}

	// Binding :80 is what lets a bay URL work without a port suffix.
	if l, err := net.Listen("tcp", "127.0.0.1:80"); err == nil {
		l.Close()
		check(true, "port 80 is available", "bay hostnames will work without a port suffix")
	} else if devbayHoldsPort80(ctx, cli) {
		// devbay's own proxy is the single most likely thing to be holding
		// :80, and reporting that as a conflict told the developer to stop the
		// component that makes their bay URLs work.
		check(true, "port 80 is held by devbay's own proxy", "bay hostnames work without a port suffix")
	} else {
		note("port 80 is in use",
			"devbay will serve bay hostnames on :8080 instead, so URLs need the port. "+
				"Stopping whatever holds :80 gives you bare hostnames.")
	}

	if _, err := os.Stat("/etc/resolver"); os.IsNotExist(err) {
		note("Safari and curl will not resolve bay hostnames",
			"Chrome and Firefox resolve *.localhost themselves and need no setup. "+
				"Everything else -- Safari, curl, Node, Go, Python -- does not, which is why devbay "+
				"gives code the 127.0.0.1 address and reserves hostnames for browsers.")
	}

	if egressEnabled() {
		check(true, "egress enforcement is on", "each service reaches only what its manifest declares")
	} else {
		note("egress enforcement is off",
			"services can reach anything the host can. Set DEVBAY_EGRESS=1 to enforce each "+
				"service's `egress:` list — which matters most while installing dependencies, "+
				"where a package lifecycle script runs code from a repository you have just cloned.")
	}

	return summarise(problems)
}

func summarise(problems int) error {
	fmt.Println()
	if problems == 0 {
		fmt.Printf("%s no blocking problems\n", green("ok"))
		return nil
	}
	return fmt.Errorf("%d blocking problem(s)", problems)
}

// isAppleVZ guesses whether the daemon is running under Apple's
// Virtualization.framework, where freed guest memory is not returned to macOS.
func isAppleVZ(osName, kernel, name string) bool {
	s := strings.ToLower(osName + " " + kernel + " " + name)
	if strings.Contains(s, "orbstack") {
		return false
	}
	return strings.Contains(s, "docker desktop") || strings.Contains(s, "linuxkit")
}

// devbayHoldsPort80 reports whether the thing occupying :80 is devbay's proxy.
//
// It is by far the most likely occupant on a machine that has run devbay
// before, and calling that a conflict advised the developer to stop the one
// component that makes bay hostnames work at all.
func devbayHoldsPort80(ctx context.Context, cli *client.Client) bool {
	list, err := cli.ContainerList(ctx, client.ContainerListOptions{
		Filters: make(client.Filters).Add("name", proxy.ContainerName),
	})
	if err != nil {
		return false
	}
	for _, c := range list.Items {
		for _, p := range c.Ports {
			if p.PublicPort == 80 {
				return true
			}
		}
	}
	return false
}

// checkDisk reports how much room the container runtime has left.
//
// Worth its own check because running out is not a clear failure: it surfaces
// from inside whatever container happened to need space first, as something
// like "initdb: could not create directory: No space left on device" buried in
// a Postgres log. A developer reads that as a broken database rather than a
// full disk, and images are the largest thing devbay creates.
func checkDisk(ctx context.Context, cli *client.Client, check func(bool, string, string), note func(string, string)) {
	du, err := cli.DiskUsage(ctx, client.DiskUsageOptions{})
	if err != nil {
		return // not worth a warning of its own; the daemon is clearly answering
	}
	images := du.Images.TotalSize
	containers := du.Containers.TotalSize
	volumes := du.Volumes.TotalSize
	cache := du.BuildCache.TotalSize
	total := images + containers + volumes + cache

	const gb = 1 << 30
	summary := fmt.Sprintf("images %.1f GiB, volumes %.1f GiB, build cache %.1f GiB",
		float64(images)/gb, float64(volumes)/gb, float64(cache)/gb)

	// How much is left matters more than how much is used, and the API cannot
	// answer it -- so ask the filesystem the containers actually write to.
	if free, ok := freeSpace(ctx, cli); ok {
		switch {
		case free < 3*gb:
			check(false, fmt.Sprintf("the container runtime has %.1f GiB free", float64(free)/gb),
				"builds and databases will fail with errors that look like application bugs -- "+
					"`no space left on device` from inside a container. "+
					"`docker system prune` and `devbay rm` on bays you are done with.")
			return
		case free < 10*gb:
			note(fmt.Sprintf("the container runtime has %.1f GiB free", float64(free)/gb),
				summary+". One large image or a seeded database will use it.")
			return
		}
		check(true, fmt.Sprintf("runtime storage %.1f GiB free", float64(free)/gb), summary)
		return
	}

	// No free-space reading available, so report what devbay can see instead.
	// A threshold rather than a hard failure: plenty of machines run happily
	// at 20 GiB.
	if total > 20*gb {
		note(fmt.Sprintf("the container runtime is holding %.1f GiB", float64(total)/gb),
			summary+". Running out shows up as an unrelated-looking error inside a container -- "+
				"`docker system df` to look, `docker builder prune` and `devbay rm` on bays you are done with to reclaim.")
		return
	}
	check(true, fmt.Sprintf("runtime storage %.1f GiB", float64(total)/gb), summary)
}

// freeSpace reads the free space on the filesystem containers write to.
//
// Through a container, because on macOS and on any non-local daemon that
// filesystem is inside a VM and the host cannot see it. This is the number
// that actually predicts the failure: usage tells you nothing without knowing
// the size of the disk, and the disk here is a VM image that the developer
// sized months ago and has not thought about since.
func freeSpace(ctx context.Context, cli *client.Client) (int64, bool) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	res, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: "busybox:stable",
			Cmd:   []string{"df", "-P", "/"},
			// A TTY so the daemon does not multiplex the output into framed
			// chunks; this reads one short line and has no use for stderr.
			Tty: true,
		},
	})
	if err != nil {
		return 0, false // not present and not worth a pull inside doctor
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.WithoutCancel(ctx), res.ID,
			client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		return 0, false
	}
	wait := cli.ContainerWait(ctx, res.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case <-wait.Error:
		return 0, false
	case <-wait.Result:
	case <-ctx.Done():
		return 0, false
	}

	logs, err := cli.ContainerLogs(ctx, res.ID, client.ContainerLogsOptions{ShowStdout: true})
	if err != nil {
		return 0, false
	}
	defer logs.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, logs); err != nil {
		return 0, false
	}
	// Filesystem 1024-blocks Used Available Capacity Mounted
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		return 0, false
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, false
	}
	blocks, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return 0, false
	}
	return blocks * 1024, true
}
