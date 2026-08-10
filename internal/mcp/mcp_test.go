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
		props, _ := tool.InputSchema["properties"].(map[string]any)
		if _, ok := props["bay"]; !ok {
			t.Errorf("%s has no bay argument; the protocol is stateless, so it cannot infer one", tool.Name)
		}
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
