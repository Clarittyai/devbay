package introspect

import (
	"strings"
	"testing"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// A repository that ships a compose file has already said how it runs.
//
// Found by running devbay on a plain Node stack: docker-compose.yml defined
// "api" and "db", and package.json's `start` script ran the very same server
// the "api" service runs. The conventions added it a second time as "web",
// which then claimed the bay hostname, had no DATABASE_URL to reach the
// database with, and exited on boot -- so a stack that composes perfectly well
// came up degraded, and the failing service was one the repository never had.
const composeAPIAndDB = `services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_PASSWORD: x
  api:
    image: node:22-alpine
    working_dir: /app
    command: node server.js
    environment:
      DATABASE_URL: postgres://postgres:x@db:5432/app
      PORT: "3000"
    ports: ["3000:3000"]
    depends_on: [db]
`

func TestComposeStopsTheConventionsInventingAService(t *testing.T) {
	res := detect(t, fixture(t, map[string]string{
		"docker-compose.yml": composeAPIAndDB,
		"package.json":       `{"name":"app","scripts":{"start":"node server.js","test":"node --test"},"dependencies":{"pg":"^8"}}`,
		"server.js":          "// the same server the api service runs\n",
	}))

	svcs := res.Manifest.Services
	if _, invented := svcs["web"]; invented {
		t.Error(`the conventions added a "web" service on top of a compose file that already runs this application`)
	}
	if len(svcs) != 2 {
		var names []string
		for n := range svcs {
			names = append(names, n)
		}
		t.Errorf("got %d services %v, want exactly the two the compose file declares", len(svcs), names)
	}
	for _, want := range []string{"api", "db"} {
		if svcs[want] == nil {
			t.Errorf("service %q from the compose file is missing", want)
		}
	}

	// Exactly one service may own the hostname, and it must be a real one.
	var primary []string
	for name, s := range svcs {
		if s.Primary {
			primary = append(primary, name)
		}
	}
	if len(primary) != 1 || primary[0] != "api" {
		t.Errorf("primary = %v, want [api]: the bay hostname must reach a service the repository actually has", primary)
	}
}

// The conventions still contribute what compose does not describe: a compose
// file says how to run the app and nothing about how to test it.
func TestComposeDoesNotSuppressTasks(t *testing.T) {
	res := detect(t, fixture(t, map[string]string{
		"docker-compose.yml": composeAPIAndDB,
		"package.json":       `{"name":"app","scripts":{"start":"node server.js","test":"node --test"}}`,
	}))
	if res.Manifest.Tasks["test"] == nil {
		var names []string
		for n := range res.Manifest.Tasks {
			names = append(names, n)
		}
		t.Errorf("the test script was dropped along with the service; tasks are %v", names)
	}
}

// Without a compose file the conventions are all there is, so they must still fire.
func TestWithoutComposeTheConventionsStillRun(t *testing.T) {
	res := detect(t, fixture(t, map[string]string{
		"package.json": `{"name":"app","scripts":{"start":"node server.js"}}`,
	}))
	if res.Manifest.Services["web"] == nil {
		t.Fatal(`no "web" service: with no compose file the package.json script is the only evidence there is`)
	}
}

// A compose file devbay cannot use describes nothing, so the conventions are
// still the best evidence available.
func TestAnUnusableComposeFileDoesNotSilenceTheConventions(t *testing.T) {
	res := detect(t, fixture(t, map[string]string{
		// An unset variable eats the tag, so the service is unusable.
		"docker-compose.yml": "services:\n  api:\n    image: \"myapp:${NOPE}\"\n",
		"package.json":       `{"name":"app","scripts":{"start":"node server.js"}}`,
	}))
	if res.Manifest.Services["web"] == nil {
		var names []string
		for n := range res.Manifest.Services {
			names = append(names, n)
		}
		t.Errorf("the compose file produced no usable service and the conventions were suppressed anyway; services are %v", names)
	}
}

// The generated file has to keep passing the validator.
func TestComposePlusPackageJSONStillValidates(t *testing.T) {
	res := detect(t, fixture(t, map[string]string{
		"docker-compose.yml": composeAPIAndDB,
		"package.json":       `{"name":"app","scripts":{"start":"node server.js"}}`,
	}))
	if r := manifest.Validate(res.Manifest); !r.OK() {
		t.Fatalf("the generated manifest does not validate: %v", r.Errors())
	}
	if s := res.Manifest.Services["api"]; s == nil || !strings.Contains(s.Env["DATABASE_URL"], "${bay.db") {
		t.Errorf("api.DATABASE_URL was not rewired to the bay's database: %+v", s)
	}
}

// Compose for the backing services and the app from package.json is a normal
// way to work, and there the conventions are the only thing that knows how to
// start the application.
func TestComposeWithOnlyDatastoresLeavesTheAppToTheConventions(t *testing.T) {
	res := detect(t, fixture(t, map[string]string{
		"docker-compose.yml": "services:\n  db:\n    image: postgres:16\n  cache:\n    image: redis:7\n",
		"package.json":       `{"name":"app","scripts":{"dev":"next dev"},"dependencies":{"next":"15"}}`,
	}))
	if res.Manifest.Services["web"] == nil {
		var names []string
		for n := range res.Manifest.Services {
			names = append(names, n)
		}
		t.Fatalf("nothing runs the application: services are %v. The compose file only brings up what it talks to", names)
	}
}
