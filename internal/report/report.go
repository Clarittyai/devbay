// Package report turns a test runner's output into typed failures.
//
// This is the difference between an agent that fixes its own test and one that
// flails. Handed raw stdout, a model has to guess at the format, and it guesses
// differently each time; handed {file, line, message} it can open the file and
// edit the line.
//
// There is no cross-language standard for this, and it is worth being blunt
// that there isn't. JUnit XML is a de facto format with real dialect drift --
// pytest ships a junit_family setting precisely because implementations
// disagree -- and the native JSON formats are each single-ecosystem. So the
// approach is one small internal shape plus a parser per format, built against
// output captured from the real runners rather than from documentation.
package report

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// Status is the outcome of one test.
type Status string

const (
	Passed  Status = "passed"
	Failed  Status = "failed"
	Skipped Status = "skipped"
)

// Failure is one failing test, located.
type Failure struct {
	// Name is the test's name, as the runner reports it.
	Name string `json:"name"`
	// Suite is the enclosing class, file, or package.
	Suite string `json:"suite,omitempty"`
	// File and Line locate the failure. Empty when the runner does not say
	// and the message does not reveal it; never guessed.
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	// Message is the assertion message, trimmed to something an agent can read
	// without a full stack trace.
	Message string `json:"message"`
	// Output is the untruncated failure text, when it differs from Message.
	Output string `json:"output,omitempty"`
}

// Result is a parsed test run.
type Result struct {
	Total    int           `json:"total"`
	Passed   int           `json:"passed"`
	Failed   int           `json:"failed"`
	Skipped  int           `json:"skipped"`
	Duration time.Duration `json:"duration_ms"`
	Failures []Failure     `json:"failures"`
}

// Parse dispatches on the manifest's declared format.
func Parse(format manifest.ReportFormat, r io.Reader) (*Result, error) {
	switch format {
	case manifest.ReportJUnit:
		return ParseJUnit(r)
	case manifest.ReportJSON:
		return ParseJestJSON(r)
	case manifest.ReportGoJSON:
		return ParseGoJSON(r)
	default:
		return nil, fmt.Errorf("report: unsupported format %q", format)
	}
}

// ---------------------------------------------------------------------------
// JUnit XML
// ---------------------------------------------------------------------------

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
	// Node's test runner puts test cases directly under <testsuites> with no
	// <testsuite> in between. Without this they are parsed as an empty
	// document and a failing run is reported as zero tests -- which reads as
	// "nothing ran" rather than "your tests failed", and is the one wrong
	// answer a test reporter must never give.
	Cases []junitCase `xml:"testcase"`
}

type junitSuite struct {
	XMLName xml.Name    `xml:"testsuite"`
	Name    string      `xml:"name,attr"`
	Time    string      `xml:"time,attr"`
	Cases   []junitCase `xml:"testcase"`
	// Nesting is legal and some runners use it.
	Suites []junitSuite `xml:"testsuite"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	File      string        `xml:"file,attr"`
	Line      int           `xml:"line,attr"`
	Time      string        `xml:"time,attr"`
	Failures  []junitDetail `xml:"failure"`
	Errors    []junitDetail `xml:"error"`
	Skipped   *junitDetail  `xml:"skipped"`
}

type junitDetail struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// ParseJUnit reads JUnit XML, tolerating the dialects real runners emit.
func ParseJUnit(r io.Reader) (*Result, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	var suites junitSuites
	if err := xml.Unmarshal(data, &suites); err != nil {
		// Some runners emit a bare <testsuite> with no <testsuites> wrapper.
		// Failing on that would reject perfectly valid output from phpunit and
		// older rspec formatters.
		var single junitSuite
		if err2 := xml.Unmarshal(data, &single); err2 != nil {
			return nil, fmt.Errorf("report: not JUnit XML: %w", err)
		}
		suites.Suites = []junitSuite{single}
	}

	// Cases at the top level belong to an implicit suite.
	if len(suites.Cases) > 0 {
		suites.Suites = append(suites.Suites, junitSuite{Cases: suites.Cases})
	}

	res := &Result{}
	var walk func(s junitSuite)
	walk = func(s junitSuite) {
		if d, err := strconv.ParseFloat(s.Time, 64); err == nil {
			if td := time.Duration(d * float64(time.Second)); td > res.Duration {
				res.Duration = td
			}
		}
		for _, c := range s.Cases {
			res.Total++
			switch {
			case c.Skipped != nil:
				res.Skipped++
			case len(c.Failures) > 0 || len(c.Errors) > 0:
				res.Failed++
				res.Failures = append(res.Failures, junitFailure(s, c))
			default:
				res.Passed++
			}
		}
		for _, nested := range s.Suites {
			walk(nested)
		}
	}
	for _, s := range suites.Suites {
		walk(s)
	}
	return res, nil
}

func junitFailure(s junitSuite, c junitCase) Failure {
	detail := junitDetail{}
	if len(c.Failures) > 0 {
		detail = c.Failures[0]
	} else if len(c.Errors) > 0 {
		detail = c.Errors[0]
	}

	f := Failure{
		Name:    c.Name,
		Suite:   firstNonEmpty(c.ClassName, s.Name),
		File:    c.File,
		Line:    c.Line,
		Message: strings.TrimSpace(detail.Message),
		Output:  strings.TrimSpace(detail.Text),
	}
	if f.Message == "" {
		f.Message = firstLine(f.Output)
	}
	// Modern pytest writes no file or line attribute; the location appears
	// only inside the failure text, as "test_sample.py:8: AssertionError".
	// Without this the most common Python setup would produce failures an
	// agent cannot navigate to.
	if f.File == "" {
		if file, line, ok := locationIn(detail.Text); ok {
			f.File, f.Line = file, line
		}
	}
	return f
}

// ---------------------------------------------------------------------------
// Jest / Vitest JSON
// ---------------------------------------------------------------------------

type jestReport struct {
	NumTotalTests   int  `json:"numTotalTests"`
	NumPassedTests  int  `json:"numPassedTests"`
	NumFailedTests  int  `json:"numFailedTests"`
	NumPendingTests int  `json:"numPendingTests"`
	Success         bool `json:"success"`
	StartTime       int64
	TestResults     []struct {
		Name             string `json:"name"` // absolute file path
		AssertionResults []struct {
			FullName        string   `json:"fullName"`
			Title           string   `json:"title"`
			Status          string   `json:"status"`
			FailureMessages []string `json:"failureMessages"`
			Duration        float64  `json:"duration"`
			Location        *struct {
				Line   int `json:"line"`
				Column int `json:"column"`
			} `json:"location"`
		} `json:"assertionResults"`
		StartTime int64 `json:"startTime"`
		EndTime   int64 `json:"endTime"`
	} `json:"testResults"`
}

// ParseJestJSON reads the JSON that `jest --json` and
// `vitest --reporter=json` emit; Vitest documents its reporter as
// Jest-compatible, so one parser serves both.
func ParseJestJSON(r io.Reader) (*Result, error) {
	var rep jestReport
	if err := json.NewDecoder(r).Decode(&rep); err != nil {
		return nil, fmt.Errorf("report: not Jest-shaped JSON: %w", err)
	}

	res := &Result{
		Total:   rep.NumTotalTests,
		Passed:  rep.NumPassedTests,
		Failed:  rep.NumFailedTests,
		Skipped: rep.NumPendingTests,
	}
	for _, file := range rep.TestResults {
		if file.EndTime > file.StartTime {
			if d := time.Duration(file.EndTime-file.StartTime) * time.Millisecond; d > res.Duration {
				res.Duration = d
			}
		}
		for _, a := range file.AssertionResults {
			if a.Status != "failed" {
				continue
			}
			msg := strings.Join(a.FailureMessages, "\n")
			f := Failure{
				Name:    firstNonEmpty(a.FullName, a.Title),
				Suite:   file.Name,
				File:    file.Name,
				Message: firstLine(stripANSI(msg)),
				Output:  strings.TrimSpace(stripANSI(msg)),
			}
			if a.Location != nil {
				f.Line = a.Location.Line
			} else if _, line, ok := locationIn(msg); ok {
				// Jest omits location unless run with --testLocationInResults,
				// so the stack trace is the fallback.
				f.Line = line
			}
			res.Failures = append(res.Failures, f)
		}
	}
	// Counts are trusted from the header when present, but a report with
	// failures and a zero count is worse than one recomputed.
	if res.Total == 0 && len(res.Failures) > 0 {
		res.Failed = len(res.Failures)
		res.Total = res.Failed
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// go test -json
// ---------------------------------------------------------------------------

type goEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// ParseGoJSON reads the newline-delimited event stream from `go test -json`.
//
// It is a stream rather than a report file, which is why it is parsed
// incrementally: a long suite reports progress as it goes instead of only at
// the end.
func ParseGoJSON(r io.Reader) (*Result, error) {
	type acc struct {
		pkg    string
		output []string
		status Status
	}
	tests := map[string]*acc{}
	var order []string

	res := &Result{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			// `go test` interleaves build errors as plain text. They are not
			// events, and dropping them silently would turn a compile failure
			// into "0 tests, all passing".
			continue
		}
		var ev goEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Test == "" {
			// Package-level event; only the elapsed time is useful.
			if ev.Action == "pass" || ev.Action == "fail" {
				if d := time.Duration(ev.Elapsed * float64(time.Second)); d > res.Duration {
					res.Duration = d
				}
			}
			continue
		}

		key := ev.Package + "." + ev.Test
		a, ok := tests[key]
		if !ok {
			a = &acc{pkg: ev.Package}
			tests[key] = a
			order = append(order, key)
		}
		switch ev.Action {
		case "output":
			a.output = append(a.output, ev.Output)
		case "pass":
			a.status = Passed
		case "fail":
			a.status = Failed
		case "skip":
			a.status = Skipped
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	for _, key := range order {
		a := tests[key]
		name := strings.TrimPrefix(key, a.pkg+".")
		res.Total++
		switch a.status {
		case Passed:
			res.Passed++
		case Skipped:
			res.Skipped++
		case Failed:
			res.Failed++
			text := strings.Join(a.output, "")
			f := Failure{
				Name:    name,
				Suite:   a.pkg,
				Output:  strings.TrimSpace(text),
				Message: goMessage(text),
			}
			if file, line, ok := locationIn(text); ok {
				f.File, f.Line = file, line
			}
			res.Failures = append(res.Failures, f)
		default:
			// No terminal event: the run was cut short. Counting it as passed
			// would report a killed suite as green.
			res.Failed++
			res.Failures = append(res.Failures, Failure{
				Name:    name,
				Suite:   a.pkg,
				Message: "test did not report a result; the run was interrupted",
				Output:  strings.TrimSpace(strings.Join(a.output, "")),
			})
		}
	}
	sort.SliceStable(res.Failures, func(i, j int) bool { return res.Failures[i].Name < res.Failures[j].Name })
	return res, nil
}

// goMessage picks the assertion line out of go test output, skipping the
// "--- FAIL:" banner that carries no information.
func goMessage(text string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--- ") || strings.HasPrefix(trimmed, "=== ") {
			continue
		}
		if m := goLocation.FindStringSubmatch(trimmed); m != nil {
			return strings.TrimSpace(m[3])
		}
		return trimmed
	}
	return ""
}

// ---------------------------------------------------------------------------
// shared
// ---------------------------------------------------------------------------

// goLocation matches "a_test.go:8: message", the shape go test and several
// other runners use.
var goLocation = regexp.MustCompile(`([\w./\\-]+\.\w+):(\d+):(.*)`)

// pyLocation matches "test_sample.py:8: AssertionError", which is where modern
// pytest puts the location now that it no longer writes file/line attributes.
var pyLocation = regexp.MustCompile(`(?m)^([\w./\\-]+\.\w+):(\d+):`)

// stackLocation matches "at Object.<anonymous> (/path/file.ts:12:34)".
var stackLocation = regexp.MustCompile(`\(?([/\w.\\-]+\.\w+):(\d+):\d+\)?`)

// locationIn finds the most specific file and line in a failure body.
func locationIn(text string) (string, int, bool) {
	for _, re := range []*regexp.Regexp{pyLocation, goLocation, stackLocation} {
		if m := re.FindStringSubmatch(text); m != nil {
			line, err := strconv.Atoi(m[2])
			if err == nil {
				return m[1], line, true
			}
		}
	}
	return "", 0, false
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// stripANSI removes colour codes. Runners colour their output when they think
// a terminal is attached, and the escapes are noise in a structured field.
func stripANSI(s string) string { return ansi.ReplaceAllString(s, "") }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
