package manifest

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads and decodes a devbay.yaml. It does not validate; call Validate.
//
// Decoding is strict: an unknown key is an error rather than being silently
// ignored, because a typo in a security-relevant key — egress, install_scripts —
// must never be read as "the author didn't set it".
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m.Path = path
	return m, nil
}

// Parse decodes a manifest from YAML bytes.
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		// R1 in its structural form: a shell string where an argv array is
		// expected fails here, in the type system, before any rule runs.
		return nil, err
	}

	applyDefaults(&m)
	// Externals become ordinary services, so everything downstream -- ports,
	// hostnames, probes, teardown -- treats them as what they are.
	if err := expandExternals(&m); err != nil {
		return nil, err
	}
	applyDefaults(&m)
	return &m, nil
}

func applyDefaults(m *Manifest) {
	for _, s := range m.Services {
		if s == nil {
			continue
		}
		if s.Kind == "" {
			s.Kind = KindService
		}
		if s.Scope == "" {
			s.Scope = ScopeBay
		}
		if s.WatchAction == "" && len(s.Watch) > 0 {
			s.WatchAction = WatchRestart
		}
		// Fork is deliberately left empty unless the service declares seed
		// data. Defaulting every service to `image` would claim that a
		// stateless web server has a data directory worth forking.
		if s.Fork == "" && s.Seed != nil {
			s.Fork = ForkImage
		}
		if s.Build != nil {
			if s.Build.Context == "" {
				s.Build.Context = "."
			}
			if s.Build.Dockerfile == "" {
				s.Build.Dockerfile = "Dockerfile"
			}
		}
	}
	for _, e := range m.Externals {
		if e != nil && e.Real == "" {
			e.Real = "gated"
		}
	}
}

// PrimaryService returns the name of the service that claims the bare
// <bay>.<project> hostname. It is inferred when exactly one long-running
// service exposes a port, and declared otherwise. Validate guarantees one
// exists, so this does not report an error.
func (m *Manifest) PrimaryService() string {
	var ported []string
	for name, s := range m.Services {
		if s == nil || s.IsOneshot() {
			continue
		}
		if s.Primary {
			return name
		}
		if s.Port != 0 {
			ported = append(ported, name)
		}
	}
	if len(ported) == 1 {
		return ported[0]
	}
	return ""
}
