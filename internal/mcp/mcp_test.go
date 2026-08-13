package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// pipe is a reader and a writer that are different objects, which is what a
// real transport looks like: stdin and stdout, or the two halves of a socket.
type pipe struct {
	in  io.Reader
	out io.Writer
}

func (p pipe) Read(b []byte) (int, error)  { return p.in.Read(b) }
func (p pipe) Write(b []byte) (int, error) { return p.out.Write(b) }

// call drives one request through the server and returns the raw response.
func call(t *testing.T, s *Server, line string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	rw := pipe{in: strings.NewReader(line + "\n"), out: &buf}

	if err := s.Serve(context.Background(), rw); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if buf.Len() == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, buf.String())
	}
	return out
}

// A server with no manager still answers protocol-level calls, which is what
// tools/list has to do before any bay exists.
func newProtocolServer() *Server { return NewServer(nil) }

func TestToolsListDescribesTheWholeSurface(t *testing.T) {
	s := newProtocolServer()
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("no tools array: %v", result)
	}

	want := map[string]bool{
		"bay_create": false, "bay_list": false, "bay_run_task": false,
		"bay_logs": false, "bay_url": false, "bay_status": false, "bay_destroy": false,
	}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name, _ := tool["name"].(string)
		if _, expected := want[name]; expected {
			want[name] = true
		}
		// A tool a model cannot understand is a tool it will not use, or will
		// use wrongly.
		if desc, _ := tool["description"].(string); len(desc) < 40 {
			t.Errorf("%s: description is too thin to guide a model: %q", name, desc)
		}
		if _, ok := tool["inputSchema"].(map[string]any); !ok {
			t.Errorf("%s: no inputSchema", name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing tool %s", name)
		}
	}
}

// The alias parameter is required on purpose: naming discipline is cheapest at
// creation time, and a model left to itself produces labels nobody can read.
func TestCreateRequiresNameAndAlias(t *testing.T) {
	s := newProtocolServer()
	for _, raw := range s.Tools() {
		if raw.Name != "bay_create" {
			continue
		}
		req, _ := raw.InputSchema["required"].([]string)
		got := map[string]bool{}
		for _, r := range req {
			got[r] = true
		}
		if !got["name"] || !got["alias"] {
			t.Errorf("bay_create required = %v, want both name and alias", req)
		}
		return
	}
	t.Fatal("bay_create not registered")
}

// The spec's stateless core means a tool call cannot depend on a prior call on
// the same connection. Every tool that acts on a bay must name it.
func TestEveryBayToolTakesAnExplicitBayArgument(t *testing.T) {
	s := newProtocolServer()
	for _, tool := range s.Tools() {
		switch tool.Name {
		case "bay_list", "bay_create":
			continue // one lists, the other names the bay it makes
		}
		// The rule is about bay state, which is what a stateless protocol
		// cannot carry between calls. The repository a server was started in
		// is not that: it is fixed for the life of the connection and there is
		// only ever one, so a tool about the repository has nothing to name.
		if !strings.HasPrefix(tool.Name, "bay_") {
			continue
		}
		props, _ := tool.InputSchema["properties"].(map[string]any)
		if _, ok := props["bay"]; !ok {
			t.Errorf("%s has no bay argument; the protocol is stateless, so it cannot infer one", tool.Name)
		}
	}
}

// The setup tools exist so an agent does not have to shell out to the CLI to
// get a repository ready, which is the thing this package claims not to be.
func TestTheSurfaceCoversGettingARepositoryReady(t *testing.T) {
	s := newProtocolServer()
	have := map[string]bool{}
	for _, tool := range s.Tools() {
		have[tool.Name] = true
	}
	for _, want := range []string{"repo_status", "repo_init", "manifest_validate"} {
		if !have[want] {
			t.Errorf("no %s tool: an agent asked to set a repository up has to shell out", want)
		}
	}
	// R2 is a checkpoint for a human. A tool that granted it would remove the
	// only thing standing between an injected manifest and an approved command.
	if have["repo_approve"] || have["manifest_approve"] {
		t.Error("approval is exposed as a tool; an agent must not be able to approve its own commands")
	}
}

func TestUnknownMethodIsRejected(t *testing.T) {
	s := newProtocolServer()
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"nonsense"}`)
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error, got %v", resp)
	}
	if code := int(e["code"].(float64)); code != codeMethodNotFound {
		t.Errorf("code = %d, want %d", code, codeMethodNotFound)
	}
}

func TestUnknownToolIsRejected(t *testing.T) {
	s := newProtocolServer()
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bay_teleport"}}`)
	if _, ok := resp["error"].(map[string]any); !ok {
		t.Fatalf("expected an error, got %v", resp)
	}
}

func TestMalformedJSONGetsAParseError(t *testing.T) {
	s := newProtocolServer()
	resp := call(t, s, `{not json`)
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error, got %v", resp)
	}
	if code := int(e["code"].(float64)); code != codeParse {
		t.Errorf("code = %d, want %d", code, codeParse)
	}
}

// A notification has no id, and the current spec forbids the server from
// initiating anything, so the correct response is silence.
func TestNotificationsGetNoResponse(t *testing.T) {
	s := newProtocolServer()
	if resp := call(t, s, `{"jsonrpc":"2.0","method":"tools/list"}`); resp != nil {
		t.Errorf("a notification should produce no response, got %v", resp)
	}
}

// A tool that ran and failed is not a protocol error. An agent has to be able
// to tell "I called this wrongly" from "the thing I asked for did not work".
func TestToolFailureIsReportedAsIsErrorNotAProtocolError(t *testing.T) {
	s := NewServer(nil)
	// bay_list with a nil manager fails inside the handler.
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bay_status","arguments":{"bay":"nope"}}}`)

	if _, isProtocolError := resp["error"]; isProtocolError {
		t.Fatalf("a failing tool must not surface as a JSON-RPC error: %v", resp)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	if result["isError"] != true {
		t.Errorf("isError = %v, want true", result["isError"])
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Error("a failing tool should explain itself in content")
	}
}

func TestBatchOfRequestsIsAnsweredInOrder(t *testing.T) {
	s := newProtocolServer()
	var buf bytes.Buffer
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	}, "\n") + "\n")

	if err := s.Serve(context.Background(), pipe{in: in, out: &buf}); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d responses, want 3:\n%s", len(lines), buf.String())
	}
	for i, want := range []float64{1, 2, 3} {
		var resp map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &resp); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if resp["id"] != want {
			t.Errorf("response %d has id %v, want %v", i, resp["id"], want)
		}
	}
}

// Responses must be newline-delimited with no embedded newlines, because that
// framing is the whole transport contract.
func TestResponsesAreOnePerLine(t *testing.T) {
	s := newProtocolServer()
	var buf bytes.Buffer
	rw := pipe{in: strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"), out: &buf}
	if err := s.Serve(context.Background(), rw); err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSuffix(buf.String(), "\n")
	if strings.Contains(body, "\n") {
		t.Errorf("a response spans multiple lines, which breaks the framing:\n%s", buf.String())
	}
}

func TestWrongProtocolVersionIsRejected(t *testing.T) {
	s := newProtocolServer()
	resp := call(t, s, `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`)
	if _, ok := resp["error"].(map[string]any); !ok {
		t.Errorf("expected an error for jsonrpc 1.0, got %v", resp)
	}
}

// The tests below open a connection the way a client does, rather than
// calling a method the way the rest of this file does. Every tool worked over
// raw JSON-RPC while no shipping client could connect at all, because none of
// them ever opened the connection first.

func TestLegacyClientCanOpenAConnection(t *testing.T) {
	s := newProtocolServer()
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}`)

	if e, ok := resp["error"].(map[string]any); ok {
		t.Fatalf("a client that opens with initialize cannot connect: %v", e)
	}
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	if got := res["protocolVersion"]; got != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want the version the client asked for", got)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("tools capability was not declared, so a client will not list tools")
	}
	if info, ok := res["serverInfo"].(map[string]any); !ok || info["name"] != "devbay" {
		t.Errorf("serverInfo = %v, want devbay named", res["serverInfo"])
	}
}

func TestUnknownLegacyVersionStillConnects(t *testing.T) {
	s := newProtocolServer()
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("a legacy client cannot fall forward, so it must still be answered: %v", resp)
	}
	if got := res["protocolVersion"]; got != protocolLegacy[0] {
		t.Errorf("protocolVersion = %v, want the newest legacy version %s", got, protocolLegacy[0])
	}
}

func TestInitializedNotificationIsSilent(t *testing.T) {
	s := newProtocolServer()
	if resp := call(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp != nil {
		t.Errorf("a notification was answered: %v", resp)
	}
}

func TestModernClientDiscoversTheServer(t *testing.T) {
	s := newProtocolServer()
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)

	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("server/discover must be implemented; got %v", resp)
	}
	versions, _ := res["supportedVersions"].([]any)
	if len(versions) == 0 || versions[0] != protocolModern {
		t.Errorf("supportedVersions = %v, want %s first", versions, protocolModern)
	}
	if res["instructions"] == "" || res["instructions"] == nil {
		t.Error("no instructions: an agent is told what it may call but not how to work")
	}
	meta, _ := res["_meta"].(map[string]any)
	if _, ok := meta["io.modelcontextprotocol/serverInfo"]; !ok {
		t.Error("serverInfo missing from _meta")
	}
}

func TestUnsupportedModernVersionNamesWhatIsSupported(t *testing.T) {
	s := newProtocolServer()
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1900-01-01"}}}`)

	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected UnsupportedProtocolVersionError, got %v", resp)
	}
	if code := int(e["code"].(float64)); code != codeUnsupportedVersion {
		t.Errorf("code = %d, want %d so the client retries instead of falling back", code, codeUnsupportedVersion)
	}
	data, _ := e["data"].(map[string]any)
	if supported, _ := data["supported"].([]any); len(supported) == 0 {
		t.Error("the error must name the versions the server does support")
	}
}

func TestASupportedVersionInMetaIsServed(t *testing.T) {
	s := newProtocolServer()
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)
	if _, ok := resp["error"]; ok {
		t.Fatalf("a supported version was rejected: %v", resp)
	}
}
