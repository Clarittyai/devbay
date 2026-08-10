// Package spec embeds the published devbay.yaml JSON Schema and exposes the
// parts of it the Go validator needs.
//
// The schema file is the single source of truth for anything a third party
// could reasonably reimplement — the argv[0] allowlist, the interpolation
// grammar, the credential screen. Reading them from the embedded document
// rather than restating them in Go keeps the published spec and the shipped
// validator from drifting apart, which is the whole reason the spec is a
// standalone artifact.
package spec

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
)

//go:embed devbay.schema.json
var Schema []byte

// Rules are the machine-checkable parts of the schema.
type Rules struct {
	Interpolated *regexp.Regexp // R7: the permitted interpolation grammar
	Credential   *regexp.Regexp // R3: known credential prefixes
	Slug         *regexp.Regexp
	Duration     *regexp.Regexp
	Allowlist    map[string]bool // R2: argv[0] values needing no approval
}

var (
	once    sync.Once
	loaded  Rules
	loadErr error
)

// Load returns the regexes and allowlist extracted from the embedded schema.
func Load() (Rules, error) {
	once.Do(func() {
		var doc struct {
			Defs struct {
				Interpolated struct {
					Pattern string `json:"pattern"`
					Not     struct {
						Pattern string `json:"pattern"`
					} `json:"not"`
				} `json:"interpolated"`
				Slug      struct{ Pattern string } `json:"slug"`
				Duration  struct{ Pattern string } `json:"duration"`
				Allowlist struct {
					Default []string `json:"default"`
				} `json:"argv0_allowlist"`
			} `json:"$defs"`
		}
		if err := json.Unmarshal(Schema, &doc); err != nil {
			loadErr = fmt.Errorf("parsing embedded schema: %w", err)
			return
		}

		compile := func(name, p string) *regexp.Regexp {
			if loadErr != nil {
				return nil
			}
			re, err := regexp.Compile(p)
			if err != nil {
				// The patterns are written without lookahead precisely so they
				// compile under RE2 as well as ECMA-262. If this fires, the
				// schema and the Go validator have diverged.
				loadErr = fmt.Errorf("schema pattern %s is not RE2-compatible: %w", name, err)
			}
			return re
		}

		loaded.Interpolated = compile("interpolated", doc.Defs.Interpolated.Pattern)
		loaded.Credential = compile("credential", doc.Defs.Interpolated.Not.Pattern)
		loaded.Slug = compile("slug", doc.Defs.Slug.Pattern)
		loaded.Duration = compile("duration", doc.Defs.Duration.Pattern)

		loaded.Allowlist = make(map[string]bool, len(doc.Defs.Allowlist.Default))
		for _, a := range doc.Defs.Allowlist.Default {
			loaded.Allowlist[a] = true
		}
		if len(loaded.Allowlist) == 0 && loadErr == nil {
			loadErr = fmt.Errorf("embedded schema has an empty argv0 allowlist")
		}
	})
	return loaded, loadErr
}
