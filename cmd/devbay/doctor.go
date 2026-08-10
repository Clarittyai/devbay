package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

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

	fmt.Printf("\nproxy image %s\n", dim(proxy.Image))
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
