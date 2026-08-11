package engine

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// Probe timing defaults. A probe that gives up too early turns a slow first
// boot into a failure; one that never gives up turns a broken service into a
// hang.
const (
	defaultTimeout  = 30 * time.Second
	defaultInterval = 500 * time.Millisecond
)

// waitHealthy blocks until a service's probe passes, its timeout expires, or
// the container exits.
func (e *Engine) waitHealthy(ctx context.Context, name, id string, s *manifest.Service) error {
	h := s.Health
	if h == nil {
		return errors.New("no health probe declared")
	}
	timeout := parseDuration(h.Timeout, defaultTimeout)
	grace := parseDuration(h.StartPeriod, 0)

	ctx, cancel := context.WithTimeout(ctx, timeout+grace)
	defer cancel()

	if grace > 0 {
		select {
		case <-time.After(grace):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// The log probe consumes a stream rather than polling, so it is its own
	// shape.
	if h.Log != "" {
		return e.probeLog(ctx, id, h.Log)
	}

	interval := parseDuration(h.Interval, defaultInterval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var last error
	// expired composes the message a developer actually needs: what was
	// probed, and what it kept answering. Written once and used from every
	// exit, because the deadline can fire anywhere in this loop -- and when it
	// fired during a Docker call, this used to return that call's own
	// "context deadline exceeded" against a docker.sock URL, which names the
	// tool's plumbing instead of the service that never came up.
	expired := func() error {
		if last != nil {
			return fmt.Errorf("did not become healthy within %s: %w", timeout, last)
		}
		return fmt.Errorf("did not become healthy within %s", timeout)
	}

	for {
		if ctx.Err() != nil {
			return expired()
		}

		// A container that has exited will never become healthy, so waiting
		// out the full timeout only delays a failure the daemon already knows
		// about.
		alive, code, err := e.running(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return expired()
			}
			return err
		}
		if !alive {
			// A service Docker will restart has not failed yet. Real compose
			// files use `restart: on-failure` where they cannot express a
			// dependency -- a web service racing the cache it talks to -- and
			// treating the first exit as fatal makes devbay fail on stacks
			// that work under compose. The health timeout is still the bound:
			// a service that crash-loops for its whole timeout reports the
			// last thing the probe saw, and the log tail says why.
			if restarting(s, code) {
				last = fmt.Errorf("exited with code %d and is being restarted", code)
				select {
				case <-ticker.C:
					continue
				case <-ctx.Done():
					return expired()
				}
			}
			return fmt.Errorf("container exited with code %d before becoming healthy", code)
		}

		if err := e.probeOnce(ctx, name, id, s); err == nil {
			return nil
		} else if ctx.Err() == nil {
			// A probe cut short by the deadline describes the cancellation
			// rather than the service, so the previous round's answer is the
			// one worth keeping.
			last = err
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return expired()
		}
	}
}

// restarting reports whether Docker will bring this service back by itself.
//
// The exit code matters: `on-failure` does not restart a container that exited
// zero, so a service that quits cleanly under that policy is finished. Saying
// "is being restarted" about a container nothing will restart sends the
// developer to wait for something that is never going to happen.
func restarting(s *manifest.Service, code int) bool {
	switch s.Restart {
	case manifest.RestartAlways, manifest.RestartUnlessStopped:
		return true
	case manifest.RestartOnFailure:
		return code != 0
	}
	return false
}

// probeOnce runs a single non-streaming probe.
func (e *Engine) probeOnce(ctx context.Context, name, id string, s *manifest.Service) error {
	h := s.Health
	switch {
	case h.Process:
		// Liveness only. Reaching here means the container is running, which
		// the caller already checked, so there is nothing further to ask.
		return nil

	case h.HTTP != "":
		// Probes always run from the host plane. Probing a *.localhost
		// hostname instead would fail regardless of the service's state,
		// because Go's resolver does not special-case those names.
		ep, err := e.res.Endpoint(name, PlaneHost)
		if err != nil {
			return err
		}
		return probeHTTP(ctx, "http://"+ep.Addr()+h.HTTP)

	case h.TCP != 0:
		port, err := e.hostPortFor(name, s, h.TCP)
		if err != nil {
			return err
		}
		return probeTCP(ctx, port)

	case len(h.Cmd) > 0:
		return e.probeExec(ctx, id, h.Cmd)
	}
	return errors.New("no probe form set")
}

// hostPortFor maps a container port named by a tcp probe to its published
// host port.
func (e *Engine) hostPortFor(name string, s *manifest.Service, containerPort int) (int, error) {
	if s.Port == containerPort {
		ep, err := e.res.Endpoint(name, PlaneHost)
		return ep.Port, err
	}
	for pn, cp := range s.Ports {
		if cp == containerPort {
			hp, ok := e.res.namedHostPort(name, pn)
			if !ok {
				return 0, fmt.Errorf("named port %s is not published", pn)
			}
			return hp, nil
		}
	}
	return 0, fmt.Errorf("tcp probe names port %d, which the service does not declare", containerPort)
}

func probeHTTP(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// No redirect following: a 302 is a healthy answer from the server, and
	// chasing it can leave the bay entirely.
	c := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return nil
	}
	return fmt.Errorf("%s returned %s", url, resp.Status)
}

// probeTCP asks whether something is actually listening behind a published
// port -- which is not the same question as whether the port accepts.
//
// Docker's userland proxy binds the host port the moment the container is
// created and accepts connections whether or not the process inside has
// started listening; when nothing is there, it accepts and immediately closes.
// A probe that dialled and hung up therefore reported a database as ready
// while it was still running initdb, and everything downstream believed it: a
// migration ran against a server that was not accepting TCP, and the failure
// surfaced as the application's error rather than as an unfinished boot.
//
// So the connection is held open briefly and read from. A server waiting for a
// request keeps the connection open and the read times out -- postgres, redis
// and mysql all behave this way -- and one that greets its clients sends a
// banner. Only an immediate EOF means the port was answered by a proxy with
// nothing behind it.
func probeTCP(ctx context.Context, port int) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		return nil // cannot tell; treat the successful connection as the answer
	}
	var b [1]byte
	switch _, err := conn.Read(b[:]); {
	case err == nil:
		return nil // it said something first, so it is up
	case errors.Is(err, os.ErrDeadlineExceeded):
		return nil // waiting for us, which is what a ready server does
	default:
		return fmt.Errorf("port %d accepted the connection and closed it at once, "+
			"which is what Docker's port forwarder does while the service is still starting", port)
	}
}

// probeExec runs a command inside the container; exit zero is healthy.
func (e *Engine) probeExec(ctx context.Context, id string, argv manifest.Argv) error {
	created, err := e.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		Cmd:          argv,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return err
	}
	att, err := e.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer att.Close()
	// Drain, bounded: an unhealthy command can be chatty, and the output is
	// only used to explain a failure.
	_, _ = io.Copy(io.Discard, io.LimitReader(att.Reader, 64<<10))

	for {
		ins, err := e.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return err
		}
		if !ins.Running {
			if ins.ExitCode == 0 {
				return nil
			}
			if ins.ExitCode == 127 {
				// Not an unhealthy service: a probe that is not in the image.
				// Exit 127 is the shell's "command not found", and the two
				// have opposite fixes -- one is the developer's application,
				// the other is this line of their manifest. Saying "exited
				// 127" leaves them debugging a database that is fine.
				return fmt.Errorf("the probe command %q is not present in this image; "+
					"change health: for this service (tcp: <port> always works)", argv[0])
			}
			return fmt.Errorf("probe command exited %d", ins.ExitCode)
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// maxProbeLine caps how much of a single log line is examined. Without a cap,
// a service that emits one enormous line — a stack trace, a base64 payload —
// would be buffered in full while devbay waits for a match it will never find.
const maxProbeLine = 64 << 10

// probeLog follows a container's output until a line matches.
//
// This is the preferred probe for a process with no port, and is often better
// than an HTTP probe for a dev server: Vite prints "ready in 412 ms", Sidekiq
// prints "Starting processing", Celery prints "ready.". Matching the line the
// process already emits is real readiness rather than a guess that a socket
// being open means the application is up.
func (e *Engine) probeLog(ctx context.Context, id, pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("health.log: %w", err)
	}

	rc, err := e.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: true,
	})
	if err != nil {
		return err
	}
	// Closing the stream is what stops the scan; the goroutine below cannot be
	// interrupted any other way.
	defer rc.Close()

	type result struct{ err error }
	done := make(chan result, 1)

	go func() {
		sc := bufio.NewScanner(demux(rc))
		sc.Buffer(make([]byte, 0, 4096), maxProbeLine)
		for sc.Scan() {
			if re.Match(sc.Bytes()) {
				done <- result{nil}
				return
			}
		}
		if err := sc.Err(); err != nil {
			done <- result{fmt.Errorf("reading logs: %w", err)}
			return
		}
		done <- result{errors.New("log stream ended before the pattern matched")}
	}()

	// The container exiting has to end the wait too, or a service that crashes
	// silently would block until the timeout.
	exited := make(chan int, 1)
	go func() {
		res := e.cli.ContainerWait(context.WithoutCancel(ctx), id, client.ContainerWaitOptions{
			Condition: container.WaitConditionNotRunning,
		})
		select {
		case st := <-res.Result:
			exited <- int(st.StatusCode)
		case <-res.Error:
		case <-ctx.Done():
		}
	}()

	select {
	case r := <-done:
		return r.err
	case code := <-exited:
		return fmt.Errorf("container exited with code %d before %q appeared in its output", code, pattern)
	case <-ctx.Done():
		return fmt.Errorf("%q did not appear in the output before the timeout", pattern)
	}
}

// running reports whether a container is still up, and its exit code if not.
func (e *Engine) running(ctx context.Context, id string) (bool, int, error) {
	ins, err := e.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return false, 0, err
	}
	st := ins.Container.State
	if st == nil {
		return false, 0, errors.New("container has no state")
	}
	return st.Running, st.ExitCode, nil
}

// demux unwraps Docker's log framing.
//
// Without a TTY the daemon multiplexes stdout and stderr into one stream with
// an 8-byte header per frame. Handing that to a line scanner unchanged would
// splice binary headers into the text, so a probe pattern could match a header
// byte or, more often, fail to match a line it should have.
func demux(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		var hdr [8]byte
		for {
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				pw.CloseWithError(err)
				return
			}
			// A valid header has a stream type of 0-2 and three zero bytes.
			if hdr[0] > 2 || hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 0 {
				// Not framed after all — a TTY stream. Pass it through.
				if _, err := pw.Write(hdr[:]); err != nil {
					return
				}
				_, err := io.Copy(pw, r)
				pw.CloseWithError(err)
				return
			}
			n := binary.BigEndian.Uint32(hdr[4:])
			if _, err := io.CopyN(pw, r, int64(n)); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()
	return pr
}
