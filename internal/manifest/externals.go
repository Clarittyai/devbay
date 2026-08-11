package manifest

import (
	"fmt"
	"sort"
)

// Emulator is a local stand-in for a third-party service.
//
// The catalogue exists so `externals:` is a decision rather than research. A
// developer writing `emulate: mailpit` should not also have to know that it
// listens on 1025 and serves its interface on 8025, and getting either wrong
// produces a bay that boots and quietly cannot send mail.
type Emulator struct {
	// Image is digest-free on purpose: these are devbay's choices rather than
	// the repository's, and pinning them here would mean a devbay release to
	// take a patch to a mail catcher.
	Image string
	// Port is the one that gets the bay hostname.
	Port int
	// Ports are the rest, each of which gets its own subdomain -- a mail
	// catcher's interface and its SMTP port are separately addressable, which
	// is the whole reason a service may declare more than one.
	Ports map[string]int
	// Health is how devbay knows it is ready.
	Health *Health
	// Cmd overrides the image's entrypoint where the emulator needs it.
	Cmd Argv
	Env map[string]string
	// Why is shown when a developer asks what they are getting.
	Why string
}

// Emulators is the catalogue. Deliberately short: an entry here is a promise
// that the image, the ports and the probe are right, and a wrong entry is
// worse than no entry because it fails inside somebody else's application.
var Emulators = map[string]Emulator{
	"mailpit": {
		Image:  "axllent/mailpit:latest",
		Port:   8025,
		Ports:  map[string]int{"smtp": 1025},
		Health: &Health{TCP: 8025},
		Why:    "catches SMTP and serves the messages at the bay's hostname",
	},
	"stripe-mock": {
		Image:  "stripe/stripe-mock:latest",
		Port:   12111,
		Health: &Health{TCP: 12111},
		Why:    "answers the Stripe API without a key or a network call",
	},
	"minio": {
		// The S3 API is the primary port, so it gets the bay hostname and the
		// HTTP probe -- which targets the primary port. The console is the
		// secondary, on its own subdomain.
		Image: "minio/minio:latest",
		Port:  9000,
		Ports: map[string]int{"console": 9001},
		Cmd:   Argv{"server", "/data", "--console-address", ":9001"},
		Health: &Health{
			HTTP: "/minio/health/live",
		},
		Env: map[string]string{
			"MINIO_ROOT_USER":     "devbay",
			"MINIO_ROOT_PASSWORD": "devbaydevbay",
		},
		Why: "S3-compatible object storage, with a console at the bay's hostname",
	},
}

// EmulatorNames lists the catalogue, sorted.
func EmulatorNames() []string {
	out := make([]string, 0, len(Emulators))
	for name := range Emulators {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// expandExternals turns each external into a service.
//
// They become ordinary services rather than a parallel concept, which is what
// makes them useful: a bay's mail catcher gets a hostname, a health probe, an
// address other services can reference as ${bay.mail.url}, and a teardown --
// all of it from machinery that already exists and is already tested. A
// separate lifecycle for emulators would be a second implementation of every
// one of those.
func expandExternals(m *Manifest) error {
	if len(m.Externals) == 0 {
		return nil
	}
	if m.Services == nil {
		m.Services = map[string]*Service{}
	}

	names := make([]string, 0, len(m.Externals))
	for name := range m.Externals {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		e := m.Externals[name]
		if e == nil || e.Emulate == "" {
			continue
		}
		spec, ok := Emulators[e.Emulate]
		if !ok {
			return fmt.Errorf("external %q: no emulator called %q; devbay knows %v. "+
				"Declare it as an ordinary service if you need something else",
				name, e.Emulate, EmulatorNames())
		}
		// An explicit service of the same name wins. The catalogue is a
		// convenience, and a developer who has written the service out has
		// already made every decision it would have made for them.
		if _, exists := m.Services[name]; exists {
			continue
		}

		env := map[string]string{}
		for k, v := range spec.Env {
			env[k] = v
		}
		ports := map[string]int{}
		for k, v := range spec.Ports {
			ports[k] = v
		}
		health := *spec.Health

		m.Services[name] = &Service{
			Image:    spec.Image,
			Start:    append(Argv(nil), spec.Cmd...),
			Port:     spec.Port,
			Ports:    ports,
			Health:   &health,
			Env:      env,
			Scope:    ScopeBay,
			Provided: true,
		}
	}
	return nil
}
