package report

import (
	"os"
	"strings"
	"testing"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// The JUnit fixture is real output from pytest, not something written to match
// the parser. That matters: modern pytest writes no file or line attribute on
// testcase, so a parser built from the JUnit documentation alone would produce
// failures an agent cannot navigate to.
func TestParsePytestJUnit(t *testing.T) {
	f, err := os.Open("testdata/pytest-junit.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	res, err := ParseJUnit(f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 4 || res.Passed != 1 || res.Failed != 2 || res.Skipped != 1 {
		t.Errorf("counts = total %d, passed %d, failed %d, skipped %d; want 4/1/2/1",
			res.Total, res.Passed, res.Failed, res.Skipped)
	}
	if len(res.Failures) != 2 {
		t.Fatalf("got %d failures, want 2", len(res.Failures))
	}

	byName := map[string]Failure{}
	for _, f := range res.Failures {
		byName[f.Name] = f
	}

	assert := byName["test_fails"]
	if assert.Message != "assert 42 == 7" {
		t.Errorf("message = %q, want %q", assert.Message, "assert 42 == 7")
	}
	// Recovered from the failure text, since pytest supplies no attribute.
	if assert.File != "test_sample.py" || assert.Line != 8 {
		t.Errorf("location = %s:%d, want test_sample.py:8", assert.File, assert.Line)
	}
	if assert.Suite != "test_sample" {
		t.Errorf("suite = %q, want test_sample", assert.Suite)
	}

	conn := byName["test_errors"]
	if !strings.Contains(conn.Message, "connection refused") {
		t.Errorf("message = %q, want it to mention the connection error", conn.Message)
	}
	if conn.File != "test_sample.py" || conn.Line != 15 {
		t.Errorf("location = %s:%d, want test_sample.py:15", conn.File, conn.Line)
	}
}

// Real output from `go test -json`, which is a stream of events rather than a
// document, so the parser has to reassemble each test from its output lines.
func TestParseGoJSON(t *testing.T) {
	f, err := os.Open("testdata/go-test.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	res, err := ParseGoJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 4 || res.Passed != 1 || res.Failed != 2 || res.Skipped != 1 {
		t.Errorf("counts = total %d, passed %d, failed %d, skipped %d; want 4/1/2/1",
			res.Total, res.Passed, res.Failed, res.Skipped)
	}

	byName := map[string]Failure{}
	for _, f := range res.Failures {
		byName[f.Name] = f
	}

	got := byName["TestFailsWithMessage"]
	if got.Message != "expected 42, got 7" {
		t.Errorf("message = %q, want %q", got.Message, "expected 42, got 7")
	}
	if got.File != "a_test.go" || got.Line != 8 {
		t.Errorf("location = %s:%d, want a_test.go:8", got.File, got.Line)
	}
	if got.Suite != "rf" {
		t.Errorf("suite = %q, want the package name", got.Suite)
	}

	fatal := byName["TestFatal"]
	if !strings.Contains(fatal.Message, "connection refused") {
		t.Errorf("message = %q", fatal.Message)
	}
	if fatal.Line != 14 {
		t.Errorf("line = %d, want 14", fatal.Line)
	}
	// The "--- FAIL:" banner carries no information and must not become the
	// message an agent tries to act on.
	for _, f := range res.Failures {
		if strings.HasPrefix(f.Message, "--- FAIL") {
			t.Errorf("%s: message is the banner, not the assertion: %q", f.Name, f.Message)
		}
	}
}

// A suite killed mid-run must not be reported as green. Counting a test with
// no terminal event as passed is the worst possible failure mode here: the
// agent concludes its change works.
func TestInterruptedGoRunIsNotReportedGreen(t *testing.T) {
	const truncated = `{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"output","Package":"p","Test":"TestA","Output":"    a_test.go:3: boom\n"}
{"Action":"run","Package":"p","Test":"TestB"}
{"Action":"output","Package":"p","Test":"TestB","Output":"working...\n"}
`
	res, err := ParseGoJSON(strings.NewReader(truncated))
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed != 0 {
		t.Errorf("passed = %d, want 0; no test reported a result", res.Passed)
	}
	if res.Failed != 2 {
		t.Errorf("failed = %d, want 2", res.Failed)
	}
	var sawInterrupted bool
	for _, f := range res.Failures {
		if strings.Contains(f.Message, "interrupted") {
			sawInterrupted = true
		}
	}
	if !sawInterrupted {
		t.Error("an interrupted run should say so rather than look like an assertion failure")
	}
}

// Build failures arrive as plain text interleaved with events. Skipping them
// silently would turn a compile error into "0 tests, all passing".
func TestGoBuildErrorDoesNotLookLikeSuccess(t *testing.T) {
	const withBuildError = `# rf [rf.test]
./a_test.go:5:2: undefined: nope
FAIL	rf [build failed]
`
	res, err := ParseGoJSON(strings.NewReader(withBuildError))
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 0 {
		t.Errorf("total = %d, want 0", res.Total)
	}
	// The caller distinguishes "no tests ran" from "all tests passed" using
	// the exit code; this only has to avoid inventing passes.
	if res.Passed != 0 {
		t.Errorf("passed = %d, want 0", res.Passed)
	}
}

func TestParseJestJSON(t *testing.T) {
	// Shaped exactly as jest --json and vitest --reporter=json emit.
	const jest = `{
	  "numTotalTests": 3,
	  "numPassedTests": 1,
	  "numFailedTests": 1,
	  "numPendingTests": 1,
	  "success": false,
	  "testResults": [{
	    "name": "/repo/src/auth.test.ts",
	    "startTime": 1000,
	    "endTime": 1450,
	    "assertionResults": [
	      {"fullName": "auth > signs in", "title": "signs in", "status": "passed", "failureMessages": []},
	      {"fullName": "auth > rejects a bad password", "title": "rejects a bad password",
	       "status": "failed",
	       "location": {"line": 42, "column": 5},
	       "failureMessages": ["\u001b[31mexpected 401, received 200\u001b[39m\n    at Object.<anonymous> (/repo/src/auth.test.ts:42:5)"]},
	      {"fullName": "auth > todo", "title": "todo", "status": "pending", "failureMessages": []}
	    ]
	  }]
	}`
	res, err := ParseJestJSON(strings.NewReader(jest))
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 3 || res.Passed != 1 || res.Failed != 1 || res.Skipped != 1 {
		t.Errorf("counts = %d/%d/%d/%d, want 3/1/1/1", res.Total, res.Passed, res.Failed, res.Skipped)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("got %d failures, want 1", len(res.Failures))
	}
	f := res.Failures[0]
	if f.Name != "auth > rejects a bad password" {
		t.Errorf("name = %q", f.Name)
	}
	if f.Line != 42 || f.File != "/repo/src/auth.test.ts" {
		t.Errorf("location = %s:%d, want /repo/src/auth.test.ts:42", f.File, f.Line)
	}
	// Runners colour their output when they think a terminal is attached, and
	// escape codes in a structured field are noise an agent has to strip.
	if strings.Contains(f.Message, "\x1b[") {
		t.Errorf("message still contains ANSI escapes: %q", f.Message)
	}
	if f.Message != "expected 401, received 200" {
		t.Errorf("message = %q", f.Message)
	}
}

// Some runners omit the <testsuites> wrapper. Rejecting that would reject
// perfectly valid phpunit and older rspec output.
func TestBareTestsuiteIsAccepted(t *testing.T) {
	const bare = `<testsuite name="s" tests="1" failures="1">
	  <testcase classname="C" name="t" file="src/x.rb" line="9">
	    <failure message="boom">stack</failure>
	  </testcase>
	</testsuite>`
	res, err := ParseJUnit(strings.NewReader(bare))
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || len(res.Failures) != 1 {
		t.Fatalf("failed = %d, failures = %d", res.Failed, len(res.Failures))
	}
	f := res.Failures[0]
	// When the runner does supply attributes, they win over text scraping.
	if f.File != "src/x.rb" || f.Line != 9 {
		t.Errorf("location = %s:%d, want src/x.rb:9", f.File, f.Line)
	}
}

func TestNestedSuitesAreFlattened(t *testing.T) {
	const nested = `<testsuites>
	  <testsuite name="outer">
	    <testcase classname="A" name="a"/>
	    <testsuite name="inner">
	      <testcase classname="B" name="b"><failure message="no">x</failure></testcase>
	    </testsuite>
	  </testsuite>
	</testsuites>`
	res, err := ParseJUnit(strings.NewReader(nested))
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 || res.Failed != 1 {
		t.Errorf("total = %d, failed = %d; want 2 and 1", res.Total, res.Failed)
	}
}

func TestParseDispatchesOnFormat(t *testing.T) {
	f, err := os.Open("testdata/go-test.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := Parse(manifest.ReportGoJSON, f); err != nil {
		t.Errorf("go-json: %v", err)
	}
	if _, err := Parse("nonsense", strings.NewReader("")); err == nil {
		t.Error("an unknown format should be an error, not silently empty")
	}
}

func TestGarbageIsRejectedNotGuessed(t *testing.T) {
	if _, err := ParseJUnit(strings.NewReader("this is not xml")); err == nil {
		t.Error("non-XML should be rejected")
	}
	if _, err := ParseJestJSON(strings.NewReader("not json")); err == nil {
		t.Error("non-JSON should be rejected")
	}
}

// Node's own test runner puts test cases directly under <testsuites>, with no
// <testsuite> in between. Parsed as an empty document, a failing run reported
// zero tests -- which reads as "nothing ran" rather than "your tests failed",
// and is the one wrong answer a test reporter must never give: an agent seeing
// no failures concludes its change worked.
//
// Captured from `node --test --test-reporter=junit` rather than hand-written,
// for the same reason as the other fixtures here.
func TestNodesJUnitDialect(t *testing.T) {
	f, err := os.Open("testdata/node-junit.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	res, err := ParseJUnit(f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed != 2 || res.Failed != 1 {
		t.Errorf("passed=%d failed=%d, want 2 and 1", res.Passed, res.Failed)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("got %d failures, want 1", len(res.Failures))
	}
	f0 := res.Failures[0]
	if f0.Name != "totals are computed" {
		t.Errorf("failure name = %q", f0.Name)
	}
	// The headline is the message attribute; the assertion itself is in the
	// element body, and that is the part anyone acts on.
	if !strings.Contains(f0.Message, "strictly equal") {
		t.Errorf("headline = %q", f0.Message)
	}
	if !strings.Contains(f0.Output, "2 !== 3") {
		t.Errorf("the assertion detail was lost: %q", f0.Output)
	}
	// The location is inside the text, as it is for pytest.
	if f0.File == "" || f0.Line == 0 {
		t.Errorf("no location parsed from the failure text: file=%q line=%d", f0.File, f0.Line)
	}
}
