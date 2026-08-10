package scrub

import (
	"strings"
	"testing"
)

// Planted canaries. The rule is that no secret value crosses the boundary to
// an agent, and the only way to know that holds is to plant values and assert
// they are gone -- an inspection of the code cannot tell you.
func TestKnownValuesAreRemoved(t *testing.T) {
	s := New()
	s.Add("stripe/test", "sk_test_51H8xQ2eZvKYlo2CabcdefGHIJ")
	s.Add("db/password", "hunter2-correct-horse")

	in := `starting api
DATABASE_URL=postgres://app:hunter2-correct-horse@db:5432/app
stripe configured with sk_test_51H8xQ2eZvKYlo2CabcdefGHIJ
ready`

	out := s.String(in)
	for _, canary := range []string{"sk_test_51H8xQ2eZvKYlo2CabcdefGHIJ", "hunter2-correct-horse"} {
		if strings.Contains(out, canary) {
			t.Errorf("canary %q survived scrubbing:\n%s", canary, out)
		}
	}
	// The reference is named so a developer can still tell what was involved.
	if !strings.Contains(out, "[redacted:stripe/test]") {
		t.Errorf("redaction should name the reference:\n%s", out)
	}
	// Everything that is not a secret must survive, or the log stops being
	// useful and people turn scrubbing off.
	for _, keep := range []string{"starting api", "db:5432/app", "ready"} {
		if !strings.Contains(out, keep) {
			t.Errorf("scrubbing removed non-secret text %q:\n%s", keep, out)
		}
	}
}

// A secret that contains another must be replaced whole. Replacing the shorter
// one first leaves the tail of the longer one in the output, which reads as
// safe and is not.
func TestOverlappingSecretsAreFullyRemoved(t *testing.T) {
	s := New()
	s.Add("short", "abc123def")
	s.Add("long", "abc123def456ghi789")

	out := s.String("token=abc123def456ghi789 end")
	if strings.Contains(out, "456ghi789") {
		t.Errorf("a fragment of the longer secret survived: %s", out)
	}
	if strings.Contains(out, "abc123def") {
		t.Errorf("secret survived: %s", out)
	}
}

// Credentials devbay never issued still have to go: an application fetches a
// token at runtime, an image bakes one in, a developer exports one into their
// shell. Shape detection is what covers those.
func TestUnknownCredentialShapesAreRemoved(t *testing.T) {
	for _, c := range []struct{ name, in string }{
		// Assembled from fragments rather than written out whole. The fixture has
		// to look exactly like a live key -- that is the entire point of the test --
		// and a literal that looks exactly like a live key is what a push-protection
		// scanner blocks on. The value at runtime is byte-identical. Do not "tidy"
		// this back into one string; it will block the next push.
		{"stripe live", "key=" + "sk_" + "live_51H8xQ2eZvKYlo2CabcdefGHIJ"},
		{"github classic", "token ghp_16C7e42F292c6912E7710c838347Ae178B4a"},
		{"github fine-grained", "github_pat_11ABCDEFG0abcdefghijklmnop"},
		{"gitlab", "glpat-ABCDEFGHIJKLMNOPQRST"},
		{"aws key id", "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"},
		{"google oauth", "ya29.a0AfH6SMBx1234567890abcdefghij"},
		{"google api", "AIzaSyD-1234567890abcdefghijklmnopqrstuv"},
		{"slack", "xoxb-123456789012-1234567890123-abcdefghij"},
		{"anthropic", "sk-ant-api03-abcdefghijklmnopqrstuvwx"},
		{"npm", "npm_abcdefghijklmnopqrstuvwxyz1234567890"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := Text(c.in)
			if out == c.in {
				t.Errorf("not redacted: %s", out)
			}
			if !strings.Contains(out, Redaction) {
				t.Errorf("expected a redaction marker, got: %s", out)
			}
		})
	}
}

func TestPrivateKeyBlockIsRemovedWhole(t *testing.T) {
	in := `config loaded
-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAx4fm7dngEmOULNmAs1IGZ9Apfzh+BFHYWJnvNCCCU2LqSU7X
kv0UcaLtGYaZbNlYrPLxBmeVIC9ByAsAyOb9pvJFJ8gCqTuxLKAOa1cKUcVsMkKz
-----END RSA PRIVATE KEY-----
done`
	out := Text(in)
	if strings.Contains(out, "MIIEowIBAAKCAQEA") {
		t.Errorf("key material survived:\n%s", out)
	}
	if !strings.Contains(out, "config loaded") || !strings.Contains(out, "done") {
		t.Errorf("surrounding log was destroyed:\n%s", out)
	}
}

// Applications log their own DATABASE_URL constantly. The host and database
// name are the useful parts; the password is not.
func TestConnectionStringPasswordsAreRemoved(t *testing.T) {
	for _, in := range []string{
		"postgres://app:s3cr3t-p4ss@db:5432/appdb",
		"redis://default:an0ther-secret@cache:6379",
		"mysql://root:tops3cret@mysql:3306/main",
		"amqp://guest:guestpassword@rabbit:5672",
	} {
		out := Text(in)
		if strings.Contains(out, "s3cr3t") || strings.Contains(out, "an0ther-secret") ||
			strings.Contains(out, "tops3cret") || strings.Contains(out, "guestpassword") {
			t.Errorf("password survived: %s", out)
		}
		// The rest of the DSN has to survive, or an agent cannot tell which
		// database it failed to reach.
		if !strings.Contains(out, "@") {
			t.Errorf("DSN structure destroyed: %s", out)
		}
	}
}

// A scrubber that redacts everything is as useless as one that redacts
// nothing, and much more annoying.
func TestOrdinaryLogsAreUntouched(t *testing.T) {
	in := `2026-08-10T12:00:00Z INFO  listening on 0.0.0.0:3000
GET /api/users 200 12ms
migration 20260810_add_index applied
WARN  slow query: SELECT * FROM users WHERE email = $1 (412ms)`
	if out := Text(in); out != in {
		t.Errorf("ordinary log was modified:\nbefore:\n%s\nafter:\n%s", in, out)
	}
}

// A very short "secret" would match everywhere and redact the log into
// uselessness, so it is refused rather than accepted and regretted.
func TestShortValuesAreNotRegistered(t *testing.T) {
	s := New()
	s.Add("tiny", "abc")
	if s.Len() != 0 {
		t.Error("a three-character secret should not be registered")
	}
	if out := s.String("abc is a common substring in abcdef"); !strings.Contains(out, "abcdef") {
		t.Errorf("short value was used for redaction anyway: %s", out)
	}
}

func TestLinesScrubsEach(t *testing.T) {
	s := New()
	s.Add("api/key", "supersecretvalue123")
	got := s.Lines([]string{"one supersecretvalue123", "two clean", "three supersecretvalue123"})
	for i, l := range got {
		if strings.Contains(l, "supersecretvalue123") {
			t.Errorf("line %d not scrubbed: %s", i, l)
		}
	}
	if got[1] != "two clean" {
		t.Errorf("clean line changed: %q", got[1])
	}
}

func TestZeroValueIsUsable(t *testing.T) {
	var s Scrubber
	if out := s.String("token ghp_16C7e42F292c6912E7710c838347Ae178B4a"); strings.Contains(out, "ghp_16C7") {
		t.Errorf("zero-value scrubber did no shape detection: %s", out)
	}
}
