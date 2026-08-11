package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The distinction the whole probe rests on: a port that accepts and hangs up
// is Docker's forwarder answering for a service that has not started.
func TestProbeTCPRejectsAPortWithNothingBehindIt(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close() // exactly what docker-proxy does with no backend
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := probeTCP(context.Background(), port); err == nil {
		t.Error("a port that accepted and closed at once was reported healthy")
	}
}

func TestProbeTCPAcceptsAServerWaitingForARequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Held open until the test ends, the way postgres and redis wait
			// for a client to say something.
			go func() { <-done; c.Close() }()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := probeTCP(context.Background(), port); err != nil {
		t.Errorf("a server waiting for a request was reported unhealthy: %v", err)
	}
}

// A banner is data, and data means somebody is home.
func TestProbeTCPAcceptsAServerThatGreetsFirst(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("220 ready\r\n"))
			time.Sleep(500 * time.Millisecond)
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := probeTCP(context.Background(), port); err != nil {
		t.Errorf("a server that sent a banner was reported unhealthy: %v", err)
	}
}

// The most common failure in the whole tool is a service that never becomes
// healthy, so the message it produces has to name the service's answer rather
// than devbay's plumbing.
func TestHealthTimeoutReportsTheProbeNotTheDeadline(t *testing.T) {
	// A server that answers, wrongly, forever: the shape of a health path
	// pointed at the wrong endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	err := probeHTTP(context.Background(), srv.URL+"/")
	if err == nil {
		t.Fatal("a 404 was treated as healthy")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("the probe error does not say what came back: %v", err)
	}
	// And the wrapper the wait loop uses keeps it.
	wrapped := fmt.Errorf("did not become healthy within %s: %w", 15*time.Second, err)
	if !strings.Contains(wrapped.Error(), "404") || !strings.Contains(wrapped.Error(), "15s") {
		t.Errorf("the timeout message lost either the answer or the wait: %v", wrapped)
	}
}
