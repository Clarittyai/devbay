// Package mcp exposes devbay to coding agents.
//
// This is the primary interface, not a wrapper around the CLI. An agent
// shelling out and parsing terminal output is lossy in both directions: it has
// to guess at formatting that changes between versions, and it loses the
// distinction between "the test failed" and "the command failed". Every tool
// here returns a typed object.
//
// Written against the 2026-07-28 spec, which changed three things that shape
// this file:
//
//   - The protocol core is stateless. There is no initialize handshake and no
//     session id, so no bay state may be keyed on a connection. Every tool
//     takes an explicit bay name.
//   - Servers cannot initiate requests. Nothing here calls out to the client;
//     logs are pulled through a tool rather than pushed.
//   - Custom transports over a reliable byte stream should reuse the stdio
//     framing, which is newline-delimited JSON-RPC. So a unix socket and stdio
//     are the same code path, and the shim binary an agent spawns is a pipe.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/Clarittyai/devbay/internal/bay"
)

// JSON-RPC 2.0 error codes, plus the one application code devbay adds.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	// codeToolError reports a tool that ran and failed, as distinct from a
	// malformed call. An agent needs to tell "I asked wrongly" from "the thing
	// I asked for did not work".
	codeToolError = -32000
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Tool is one callable capability.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`

	handler func(context.Context, json.RawMessage) (any, error)
}

// Server serves the devbay tool surface.
type Server struct {
	mgr   *bay.Manager
	tools []Tool
	index map[string]*Tool

	mu sync.Mutex
}

// NewServer builds a server over a bay manager.
func NewServer(mgr *bay.Manager) *Server {
	s := &Server{mgr: mgr, index: map[string]*Tool{}}
	s.register()
	return s
}

// Tools returns the registered tools.
func (s *Server) Tools() []Tool { return s.tools }

// Serve runs the JSON-RPC loop over one connection.
func (s *Server) Serve(ctx context.Context, rw io.ReadWriter) error {
	dec := bufio.NewScanner(rw)
	// A tool result can be large -- a failing suite's output -- so the line
	// limit is generous. Requests are small; this bound is for symmetry with
	// what the same framing carries back.
	dec.Buffer(make([]byte, 0, 64<<10), 16<<20)

	enc := json.NewEncoder(rw)
	for dec.Scan() {
		line := dec.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(response{JSONRPC: "2.0", Error: &rpcError{
				Code: codeParse, Message: "invalid JSON: " + err.Error(),
			}})
			continue
		}

		resp := s.dispatch(ctx, &req)
		if resp == nil {
			continue // a notification; the spec says answer nothing
		}
		s.mu.Lock()
		err := enc.Encode(resp)
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return dec.Err()
}

func (s *Server) dispatch(ctx context.Context, req *request) *response {
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return errorFor(req.ID, codeInvalidRequest, "unsupported jsonrpc version "+req.JSONRPC)
	}
	// No id means a notification. Servers may not initiate requests under the
	// current spec, so there is nothing to send back.
	notification := len(req.ID) == 0

	switch req.Method {
	case "tools/list":
		if notification {
			return nil
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": s.tools}}

	case "tools/call":
		if notification {
			return nil
		}
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorFor(req.ID, codeInvalidParams, err.Error())
		}
		tool, ok := s.index[params.Name]
		if !ok {
			return errorFor(req.ID, codeMethodNotFound, "no tool named "+params.Name)
		}
		out, err := s.invoke(ctx, tool, params.Arguments)
		if err != nil {
			// A tool that ran and failed reports isError with the message as
			// content, so an agent can read the failure and act on it rather
			// than treating it as a protocol fault.
			return &response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"isError": true,
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
			}}
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Result: toolResult(out)}

	case "ping":
		if notification {
			return nil
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}

	default:
		if notification {
			return nil
		}
		return errorFor(req.ID, codeMethodNotFound, "unknown method "+req.Method)
	}
}

// invoke runs a tool handler, converting a panic into an error.
//
// One daemon serves every agent session on the machine, so a panic in a single
// tool call must not take down the others. A crashed daemon also strands the
// containers, worktrees and port blocks it was tracking, which turns a bug in
// one handler into cleanup work across the whole machine.
func (s *Server) invoke(ctx context.Context, tool *Tool, args json.RawMessage) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s panicked: %v", tool.Name, r)
		}
	}()
	return tool.handler(ctx, args)
}

// toolResult wraps a value in the content shape, carrying the structured form
// alongside. The text is what a model reads; structuredContent is what a
// harness can parse without going through prose.
func toolResult(v any) map[string]any {
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		blob = []byte(fmt.Sprintf("%v", v))
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(blob)}},
		"structuredContent": v,
		"isError":           false,
	}
}

func errorFor(id json.RawMessage, code int, msg string) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// ListenUnix serves on a unix socket until the context is cancelled.
//
// One daemon owns the ports, containers and worktrees for a machine, and many
// agent sessions connect to it. A socket rather than a per-session process is
// what makes that possible.
func (s *Server) ListenUnix(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// A socket left by a crashed daemon would refuse to bind. Removing it is
	// safe because a live daemon holds a lock on its state database.
	_ = os.Remove(path)

	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	// The socket is a full control channel over containers and credentials, so
	// nobody else on the machine gets to open it.
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return err
	}
	defer l.Close()
	defer os.Remove(path)

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			defer conn.Close()
			_ = s.Serve(ctx, conn)
		}()
	}
}

// SocketPath is the default control socket.
func SocketPath() string {
	if p := os.Getenv("DEVBAY_SOCKET"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "devbay.sock")
	}
	return filepath.Join(home, ".devbay", "devbay.sock")
}
