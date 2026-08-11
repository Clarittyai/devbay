package engine

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/manifest"
	"github.com/Clarittyai/devbay/internal/testutil"
)

func engineAt(t *testing.T, worktree string) *Engine {
	t.Helper()
	return &Engine{
		worktree: worktree,
		m:        &manifest.Manifest{Project: "acme"},
		Log:      func(string, ...any) {},
	}
}

// A build context is manifest content, and a manifest may have been generated
// from repository content. `context: ../../..` therefore has to be refused
// rather than trusted: it would hand the daemon everything above the checkout,
// which on a normal machine is the developer's home directory.
func TestABuildContextCannotEscapeTheWorktree(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(filepath.Join(worktree, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Something worth stealing, one level up.
	if err := os.WriteFile(filepath.Join(root, "secrets.txt"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := engineAt(t, worktree)

	for _, escape := range []string{"..", "../", "../..", "app/../..", "/etc"} {
		if _, err := e.buildContext(&manifest.Service{Build: &manifest.Build{Context: escape}}); err == nil {
			t.Errorf("context %q was accepted; it reaches outside the worktree", escape)
		}
	}

	// And the legitimate cases still work.
	for _, ok := range []string{"", ".", "app", "./app"} {
		if _, err := e.buildContext(&manifest.Service{Build: &manifest.Build{Context: ok}}); err != nil {
			t.Errorf("context %q was refused: %v", ok, err)
		}
	}
}

// A symlink is the version of the same attack that a prefix check alone misses.
func TestASymlinkedContextCannotEscapeEither(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{worktree, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(worktree, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	e := engineAt(t, worktree)
	if _, err := e.buildContext(&manifest.Service{Build: &manifest.Build{Context: "escape"}}); err == nil {
		t.Error("a symlink out of the worktree was accepted as a build context")
	}
}

// The tag is a cache key, so identical trees must agree and different ones
// must not -- otherwise two bays on two branches share one image.
func TestTheBuildTagFollowsTheContent(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := engineAt(t, dir)

	write("FROM alpine\n")
	first, err := e.buildTag("web", dir, "Dockerfile", "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.buildTag("web", dir, "Dockerfile", "")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Errorf("an unchanged tree produced two tags: %s and %s", first, again)
	}

	write("FROM alpine\nRUN true\n")
	changed, err := e.buildTag("web", dir, "Dockerfile", "")
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Error("a changed tree reused the old tag, so the old image would be served")
	}

	// A different stage of the same Dockerfile is a different image.
	target, err := e.buildTag("web", dir, "Dockerfile", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if target == changed {
		t.Error("build target does not affect the tag")
	}

	if !strings.HasPrefix(first, "devbay-acme-web:") {
		t.Errorf("tag %q should name the project and service so it is recognisable on the machine", first)
	}
}

// .git changes on every commit while the tree does not, so including it would
// make every commit a cache miss.
func TestTheFingerprintIgnoresNoiseDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := hashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, noise := range []string{".git", "node_modules"} {
		if err := os.MkdirAll(filepath.Join(dir, noise), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, noise, "junk"), []byte("y"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	after, err := hashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Error("the fingerprint changed because of .git or node_modules, so every commit would rebuild")
	}
}

// A missing Dockerfile is reported as a missing Dockerfile, not as a build
// failure a hundred lines into the daemon's output.
func TestAMissingDockerfileIsReportedEarly(t *testing.T) {
	dir := t.TempDir()
	e := engineAt(t, dir)
	_, err := e.buildImage(context.Background(), "web", &manifest.Service{Build: &manifest.Build{}})
	if err == nil || !strings.Contains(err.Error(), "no Dockerfile") {
		t.Errorf("err = %v, want a missing-Dockerfile message", err)
	}
}

// The daemon reports build failures inside the stream with a 200 status, so a
// caller checking only the HTTP error sees a successful build that produced
// no image.
func TestABuildFailureInTheStreamIsAnError(t *testing.T) {
	body := `{"stream":"Step 1/2 : FROM alpine\n"}
{"stream":"Step 2/2 : RUN exit 1\n"}
{"errorDetail":{"message":"The command '/bin/sh -c exit 1' returned a non-zero code: 1"},"error":"The command failed"}`
	err := drainBuild(strings.NewReader(body), func(string) {})
	if err == nil {
		t.Fatal("a failed build was reported as success")
	}
	if !strings.Contains(err.Error(), "non-zero code") {
		t.Errorf("the daemon's own reason was lost: %v", err)
	}
	// And it says where, because "the command failed" alone is not actionable.
	if !strings.Contains(err.Error(), "Step 2/2") {
		t.Errorf("the failing step is not named: %v", err)
	}
}

func TestASuccessfulBuildStreamIsNotAnError(t *testing.T) {
	body := `{"stream":"Step 1/1 : FROM alpine\n"}
{"stream":"Successfully built abc123\n"}`
	if err := drainBuild(strings.NewReader(body), func(string) {}); err != nil {
		t.Errorf("a successful build was reported as a failure: %v", err)
	}
}

// ---------------------------------------------------------------------------
// against a real daemon
// ---------------------------------------------------------------------------

const buildManifest = `
version: 1
project: buildtest
services:
  web:
    build: ./web
    port: 80
    primary: true
    health: {http: /}
tasks:
  unit: {run: ["true"], needs: []}
`

// The claim: a service that builds from source boots. Most repositories
// describe their own application with a Dockerfile rather than a published
// image, so a devbay that could only pull could not run the application it was
// pointed at -- only its database and its cache.
func TestAServiceThatBuildsFromSourceBoots(t *testing.T) {
	cli := dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	worktree := t.TempDir()
	testutil.ReclaimOnCleanup(t, cli, worktree)
	web := filepath.Join(worktree, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	// Content in the image proves the build used *this* context rather than a
	// stale layer from somewhere else.
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("built from source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "Dockerfile"),
		[]byte("FROM nginx:alpine\nCOPY index.html /usr/share/nginx/html/index.html\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := manifest.Parse([]byte(buildManifest))
	if err != nil {
		t.Fatal(err)
	}
	if r := manifest.Validate(m); !r.OK() {
		t.Fatalf("the manifest is invalid: %v", r.Err())
	}

	e, err := New(ctx, Options{
		Manifest: m, Bay: "b1", Worktree: worktree,
		Log: func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_ = e.Down(c)
	})

	plan, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Up(ctx, plan); err != nil {
		t.Fatalf("a service with build: did not come up: %v", err)
	}

	// It serves what the context contained.
	ep, err := e.Resolver().Endpoint("web", PlaneHost)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + ep.Addr() + "/")
	if err != nil {
		t.Fatalf("built service unreachable: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if !strings.Contains(string(body), "built from source") {
		t.Errorf("served %q; the build context was not the one on disk", body)
	}

	// Teardown takes the image with it. An image is the largest thing a bay
	// creates, so leaving them behind fills a disk faster than any container
	// or volume -- and nothing on the machine says which bay they came from.
	built := imagesFor(t, cli, ctx, "buildtest", "b1")
	if len(built) == 0 {
		t.Fatal("the built image carries no devbay labels, so teardown cannot find it")
	}
	if err := e.Down(ctx); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if left := imagesFor(t, cli, ctx, "buildtest", "b1"); len(left) != 0 {
		t.Errorf("%d built image(s) survived teardown", len(left))
	}
}

func imagesFor(t *testing.T, cli *client.Client, ctx context.Context, project, bay string) []string {
	t.Helper()
	_ = bay // images are project-scoped: a content-addressed tag is shared
	f := make(client.Filters).
		Add("label", LabelManaged+"=1").
		Add("label", LabelProject+"="+project)
	list, err := cli.ImageList(ctx, client.ImageListOptions{All: true, Filters: f})
	if err != nil {
		t.Fatalf("listing images: %v", err)
	}
	out := make([]string, 0, len(list.Items))
	for _, i := range list.Items {
		out = append(out, i.ID)
	}
	return out
}

// A dockerfile or target that looks like a flag must not become one. These
// values come from the manifest, and the manifest may be generated.
func TestBuildArgumentsCannotSmuggleFlags(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := engineAt(t, dir)
	for _, tc := range []struct{ tag, dockerfile, target string }{
		{"--tag=evil", "Dockerfile", ""},
		{"ok:1", "--file=/etc/passwd", ""},
		{"ok:1", "Dockerfile", "--privileged"},
	} {
		err := e.runBuild(context.Background(), "web", tc.tag, dir, tc.dockerfile, tc.target)
		if err == nil || !strings.Contains(err.Error(), "may not begin with a dash") {
			t.Errorf("runBuild(%q,%q,%q) = %v; want a refusal", tc.tag, tc.dockerfile, tc.target, err)
		}
	}
}

// The claim behind `watch:`: an edit on the host reaches the container.
//
// It has to be devbay doing the watching. The FUSE and virtiofs inotify
// patches were never merged, so a host edit does not reliably produce an
// inotify event inside the container -- which is why every guide tells people
// to turn on polling, and why five bays of a JavaScript monorepo spin a fan
// for nothing. No polling is configured here.
func TestAHostEditReachesARunningContainer(t *testing.T) {
	cli := dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	worktree := t.TempDir()
	testutil.ReclaimOnCleanup(t, cli, worktree)
	web := filepath.Join(worktree, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "Dockerfile"),
		[]byte("FROM nginx:alpine\nCOPY index.html /usr/share/nginx/html/index.html\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := manifest.Parse([]byte(`
version: 1
project: watchtest
services:
  web:
    build: ./web
    port: 80
    primary: true
    watch: ["web/**"]
    watch_action: rebuild
    health: {http: /}
tasks:
  unit: {run: ["true"], needs: []}
`))
	if err != nil {
		t.Fatal(err)
	}
	if r := manifest.Validate(m); !r.OK() {
		t.Fatalf("manifest: %v", r.Err())
	}

	e, err := New(ctx, Options{Manifest: m, Bay: "w1", Worktree: worktree,
		Log: func(f string, a ...any) { t.Logf(f, a...) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_ = e.Down(c)
	})
	plan, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Up(ctx, plan); err != nil {
		t.Fatal(err)
	}

	serves := func() string {
		t.Helper()
		ep, err := e.Resolver().Endpoint("web", PlaneHost)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get("http://" + ep.Addr() + "/")
		if err != nil {
			t.Fatalf("unreachable: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return strings.TrimSpace(string(body))
	}
	if got := serves(); got != "before" {
		t.Fatalf("served %q before any edit", got)
	}

	// The edit, on the host, exactly as an editor would make it.
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Reload(ctx, "web"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := serves(); got != "after" {
		t.Errorf("served %q after the edit and reload; the change did not reach the container", got)
	}
}
