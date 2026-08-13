package engine

import (
	"strings"
	"testing"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// The DSN devbay hands an application has to be one the database accepts.
//
// Found by running devbay on a plain compose stack: `postgres:16-alpine` with
// POSTGRES_PASSWORD and no POSTGRES_USER, which is how most compose files are
// written, because the image defaults the superuser to "postgres". devbay only
// emitted credentials when a user was named, so DATABASE_URL arrived as
// postgres://db:5432/taskflow and the application died on boot with "no
// PostgreSQL user name specified in startup packet" -- an error about the
// application, from a connection string the application never wrote.
func TestDSNCredentialsFollowTheImageDefaults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image string
		env   map[string]string
		want  string
	}{
		{
			name:  "postgres with only a password",
			image: "postgres:16-alpine",
			env:   map[string]string{"POSTGRES_PASSWORD": "taskflow", "POSTGRES_DB": "taskflow"},
			want:  "postgres://postgres:taskflow@",
		},
		{
			name:  "postgres with both",
			image: "postgres:16",
			env:   map[string]string{"POSTGRES_USER": "app", "POSTGRES_PASSWORD": "s3cret", "POSTGRES_DB": "app"},
			want:  "postgres://app:s3cret@",
		},
		{
			name:  "mysql falls back to root",
			image: "mysql:8",
			env:   map[string]string{"MYSQL_ROOT_PASSWORD": "rootpw", "MYSQL_DATABASE": "app"},
			want:  "mysql://root:rootpw@",
		},
		{
			name:  "a password with URL syntax in it is escaped",
			image: "postgres:16",
			env:   map[string]string{"POSTGRES_PASSWORD": "p@ss/word", "POSTGRES_DB": "app"},
			want:  "postgres://postgres:p%40ss%2Fword@",
		},
		{
			name:  "no credentials at all stays credential-free",
			image: "redis:7",
			env:   nil,
			want:  "redis://",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dsnFor(t, tc.image, tc.env)
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("url = %q, want it to start %q", got, tc.want)
			}
		})
	}
}

// Mongo starts without authentication unless both halves are given, so half a
// pair must not become a credential the application then fails to present.
func TestAHalfFilledMongoPairIsNotACredential(t *testing.T) {
	got := dsnFor(t, "mongo:7", map[string]string{"MONGO_INITDB_ROOT_USERNAME": "root"})
	if strings.Contains(got, "@") {
		t.Errorf("url = %q, want no credentials: mongo was given a username and no password", got)
	}
}

func dsnFor(t *testing.T, image string, env map[string]string) string {
	t.Helper()
	m := &manifest.Manifest{
		Version:  1,
		Project:  "p",
		Services: map[string]*manifest.Service{"db": {Image: image, Port: 5432, Env: env}},
	}
	r := NewResolver(m, "bay")
	r.SetHostPort("db", 40100)
	out, err := r.ResolveString("${bay.db.url}", PlaneContainer)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
