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
//
// The server is nevertheless dual-era, because "the spec removed the
// handshake" and "the clients removed the handshake" are not the same date.
// Every shipping client still opens with `initialize`, and a modern-only
// server is unreachable from all of them: the stdio fallback probe is
// `server/discover`, and answering it with "unknown method" is not a
// recognized modern error, so a dual-era client concludes the server is
// legacy, falls back to `initialize`, and fails there too. Both doors are
// therefore open. Which one the client used selects nothing and is not
// remembered -- there is no session state to key on either way, so serving
// both costs a version string rather than an architecture.
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
	// codeUnsupportedVersion is the 2026-07-28 UnsupportedProtocolVersionError.
	// A modern client reads it as "this is a modern server, retry with a
	// version it named" rather than as "this server is broken".
	codeUnsupportedVersion = -32022
)

// Protocol versions this server speaks. Modern first: it is what the server
// prefers, and what a client picks when it takes the first supported entry.
//
// The legacy entries are the revisions that still carry an `initialize`
// handshake. They are listed because a legacy client cannot fall forward --
// it has no mechanism to discover a newer version -- so the only way it ever
// reaches these tools is for the server to meet it where it is.
var (
	protocolModern = "2026-07-28"
	protocolLegacy = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
)

// supportedVersions is every version the server will serve, newest first.
func supportedVersions() []string {
	return append([]string{protocolModern}, protocolLegacy...)
}

// Version is the devbay build, reported as serverInfo. Set by main; the
// protocol says serverInfo is for display and logging only, so a client that
// cannot read it loses nothing but a line in a log.
var Version = "dev"

// serverInfo is the identity reported to both eras.
func serverInfo() map[string]any {
	return map[string]any{"name": "devbay", "version": Version}
}

// instructions tell a model how to work, not merely what it may call. The
// tool descriptions cover each call; this covers the order they go in, which
// is where an agent that has never seen devbay tends to go wrong -- usually by
// running the repository's test command itself, in the checkout everything
// else is also using.
const instructions = `devbay gives each piece of work its own running copy of this repository -- its own worktree, containers, database, ports and browser origin -- so two changes in progress cannot disturb each other.

Work in this order:
  0. repo_status first, if you do not know how this repository is set up. It says whether devbay can run bays here yet, and repo_init proposes a devbay.yaml when it cannot. manifest_validate checks an edit before you write it.
  1. bay_create once per task, before changing anything.
  2. bay_run_task to verify. Prefer it over running the test command yourself: it starts only the services the task declares, and returns failures with file, line and message rather than output to parse.
  3. bay_url when something has to be opened. Use url for code and public_url for a browser; they are different addresses and are not interchangeable.
  4. bay_logs when a task failed for a reason its failures do not explain.
  5. bay_destroy when the work is merged or abandoned. Committed work on the branch survives; uncommitted work does not.`

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
	s.registerSetup()
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

	// A modern request declares its protocol version per request. One this
	// build does not speak must be refused with the versions that it does, so
	// the client can retry rather than guess -- and refused before the call
	// runs, because a tool that creates containers is not a safe way to find
	// out the two ends disagree.
	if v := requestedVersion(req.Params); v != "" && !supports(v) {
		if notification {
			return nil
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{
			Code:    codeUnsupportedVersion,
			Message: "Unsupported protocol version",
			Data: map[string]any{
				"supported": supportedVersions(),
				"requested": v,
			},
		}}
	}

	switch req.Method {
	// The modern discovery call. Servers MUST implement it, and on stdio it is
	// also the probe a dual-era client sends first: answering it is what
	// identifies this server as modern, so the client stays modern instead of
	// falling back.
	case "server/discover":
		if notification {
			return nil
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"resultType":        "complete",
			"supportedVersions": supportedVersions(),
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"instructions":      instructions,
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": serverInfo(),
			},
		}}

	// The legacy handshake. Answered so that every client shipping today can
	// reach the tools; nothing is remembered about the connection as a result.
	case "initialize":
		if notification {
			return nil
		}
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		// A malformed params block is not worth failing the handshake over:
		// the version is the only field that changes the answer, and the
		// fallback below is the right answer without it.
		_ = json.Unmarshal(req.Params, &params)
		return &response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": negotiate(params.ProtocolVersion),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      serverInfo(),
			"instructions":    instructions,
		}}

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

// requestedVersion reads the protocol version a modern request declares in
// its `_meta`. Empty when absent, which is every legacy request and is not an
// error: the era is inferred from the call, never demanded.
func requestedVersion(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var p struct {
		Meta struct {
			Version string `json:"io.modelcontextprotocol/protocolVersion"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	return p.Meta.Version
}

func supports(version string) bool {
	for _, v := range supportedVersions() {
		if v == version {
			return true
		}
	}
	return false
}

// negotiate picks the protocol version to answer a legacy `initialize` with.
//
// Echoing a version the server actually supports is the whole of the legacy
// negotiation. A client asking for something unknown -- a revision newer than
// this build, or a typo -- gets the newest legacy version instead of an error,
// because a legacy client has no way to retry with a different one and would
// otherwise simply fail to connect.
func negotiate(requested string) string {
	for _, v := range supportedVersions() {
		if requested == v {
			return v
		}
	}
	return protocolLegacy[0]
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
