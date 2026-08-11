package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Build information, injected at link time by the release build:
//
//	-ldflags "-X main.version=v0.1.0 -X main.commit=abc1234 -X main.date=..."
//
// Left as "dev" for a `go build` or `go install` from source, where the
// fallback below recovers the real values from the module's build info. A
// binary that cannot say which commit it is makes a bug report guesswork.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func cmdVersion() {
	v, c, d := version, commit, date
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			case "vcs.modified":
				// Go already suffixes the version it derives from VCS, so
				// appending unconditionally produced "v0.1.0+dirty+dirty" --
				// which reads as a broken build rather than an uncommitted one.
				if s.Value == "true" && !strings.HasSuffix(v, "+dirty") {
					v += "+dirty"
				}
			}
		}
	}

	fmt.Printf("devbay %s\n", v)
	if c != "" {
		if len(c) > 12 {
			c = c[:12]
		}
		fmt.Printf("  commit  %s\n", c)
	}
	if d != "" {
		fmt.Printf("  built   %s\n", d)
	}
	fmt.Printf("  go      %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
