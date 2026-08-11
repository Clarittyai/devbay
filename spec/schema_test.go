package spec_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/Clarittyai/devbay/internal/manifest"
	"github.com/Clarittyai/devbay/spec"
)

// compiled returns the published schema, compiled.
func compiled(t *testing.T) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(spec.Schema))
	if err != nil {
		t.Fatalf("the published schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("devbay.schema.json", doc); err != nil {
		t.Fatalf("the published schema is not a valid JSON Schema: %v", err)
	}
	s, err := c.Compile("devbay.schema.json")
	if err != nil {
		t.Fatalf("compiling the published schema: %v", err)
	}
	return s
}

// asJSON converts a YAML manifest to the shape a JSON Schema validator wants.
func asJSON(t *testing.T, path string) any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	// A round-trip through JSON, because YAML decodes maps in a form the
	// validator does not accept and numbers as int rather than float64.
	blob, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The published schema is the artifact a third party would reimplement
// against, so a manifest devbay accepts has to be one the schema accepts too.
// Nothing else checks that: the Go validator reads its regexes and allowlist
// from the schema, which stops those from drifting, but says nothing about the
// document's own shape -- a field added to the types and forgotten in the
// schema would make every devbay manifest invalid to everyone else.
func TestTheFixturesValidateAgainstThePublishedSchema(t *testing.T) {
	sch := compiled(t)

	paths, err := filepath.Glob(filepath.Join("..", "testdata", "repos", "*", "devbay.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 5 {
		t.Fatalf("found %d fixture manifests; the gate is meant to cover five dissimilar repositories", len(paths))
	}

	for _, p := range paths {
		name := filepath.Base(filepath.Dir(p))
		t.Run(name, func(t *testing.T) {
			// Accepted by the implementation...
			m, err := manifest.Load(p)
			if err != nil {
				t.Fatalf("the Go parser rejected it: %v", err)
			}
			if r := manifest.Validate(m); len(r.Errors()) > 0 {
				t.Fatalf("the Go validator rejected it: %v", r.Err())
			}
			// ...must also be accepted by the published spec.
			if err := sch.Validate(asJSON(t, p)); err != nil {
				t.Errorf("the published schema rejected a manifest devbay accepts:\n%v", err)
			}
		})
	}
}

// The constructs added since the schema was first written are the ones most
// likely to exist in the Go types and not in the published document.
func TestRecentlyAddedConstructsAreInThePublishedSchema(t *testing.T) {
	sch := compiled(t)

	for _, tc := range []struct {
		name, body string
	}{
		{"build shorthand", `
version: 1
project: p
services:
  web: {build: ./web, port: 3000, primary: true, health: {http: /}}
tasks:
  unit: {run: ["true"], needs: []}
`},
		{"build mapping", `
version: 1
project: p
services:
  web:
    build: {context: ./web, dockerfile: Dockerfile.dev, target: dev}
    port: 3000
    primary: true
    health: {http: /}
tasks:
  unit: {run: ["true"], needs: []}
`},
		{"mounts", `
version: 1
project: p
services:
  web:
    build: ./web
    port: 3000
    primary: true
    mounts: [{source: ./web, target: /srv}]
    health: {http: /}
tasks:
  unit: {run: ["true"], needs: []}
`},
		{"watch", `
version: 1
project: p
services:
  web:
    build: ./web
    port: 3000
    primary: true
    watch: ["web/**"]
    watch_action: rebuild
    health: {http: /}
tasks:
  unit: {run: ["true"], needs: []}
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := manifest.Parse([]byte(tc.body))
			if err != nil {
				t.Fatalf("the Go parser rejected it: %v", err)
			}
			if r := manifest.Validate(m); len(r.Errors()) > 0 {
				t.Fatalf("the Go validator rejected it: %v", r.Err())
			}

			var doc any
			if err := yaml.Unmarshal([]byte(tc.body), &doc); err != nil {
				t.Fatal(err)
			}
			blob, _ := json.Marshal(doc)
			var out any
			_ = json.Unmarshal(blob, &out)

			if err := sch.Validate(out); err != nil {
				t.Errorf("the published schema does not describe %s, which devbay accepts:\n%v", tc.name, err)
			}
		})
	}
}
