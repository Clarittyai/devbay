package engine

import "testing"

func TestNormalizeImageRef(t *testing.T) {
	for in, want := range map[string]string{
		"nginx":                        "nginx:latest",
		"nginx:alpine":                 "nginx:alpine",
		"library/redis":                "library/redis:latest",
		"ghcr.io/org/app":              "ghcr.io/org/app:latest",
		"ghcr.io/org/app:v2":           "ghcr.io/org/app:v2",
		"localhost:5000/app":           "localhost:5000/app:latest",
		"localhost:5000/app:dev":       "localhost:5000/app:dev",
		"postgres@sha256:abc":          "postgres@sha256:abc",
		"registry:5000/a/b@sha256:abc": "registry:5000/a/b@sha256:abc",
	} {
		if got := normalizeImageRef(in); got != want {
			t.Errorf("normalizeImageRef(%q) = %q, want %q", in, got, want)
		}
	}
}
