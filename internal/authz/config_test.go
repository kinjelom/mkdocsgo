package authz

import (
	"fmt"
	"strings"
	"testing"
)

// A configuration good enough to parse, with the parts a test wants to break
// filled in by the caller.
func parse(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	return Parse([]byte(yaml), "mkdocsgo.yml")
}

func mustParse(t *testing.T, yaml string) *Config {
	t.Helper()
	config, err := parse(t, yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return config
}

const oneToken = `version: 1
zones:
  internet:
    hosts: [docs.example.com]
    access: restricted
    principals: [reader]
principals:
  reader:
    tokens:
      - id: t1
        hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
`

func TestTheMostSpecificHostWins(t *testing.T) {
	config := mustParse(t, `version: 1
zones:
  exact:
    hosts: [a.docs.example.com]
    access: public
  inner:
    hosts: ["*.docs.example.com"]
    access: public
  outer:
    hosts: ["*.example.com"]
    access: public
  rest:
    hosts: ["*"]
    access: off
`)
	for host, want := range map[string]string{
		"a.docs.example.com":   "exact",
		"b.docs.example.com":   "inner",
		"anything.example.com": "outer",
		"elsewhere.test":       "rest",
	} {
		zone, ok := config.Zone(host)
		if !ok {
			t.Fatalf("%s matched no zone", host)
		}
		if zone.Name != want {
			t.Errorf("%s matched zone %q, want %q", host, zone.Name, want)
		}
	}
}

func TestAWildcardCoversOneLabelOnly(t *testing.T) {
	config := mustParse(t, `version: 1
zones:
  inner:
    hosts: ["*.docs.example.com"]
    access: public
`)
	if _, ok := config.Zone("a.b.docs.example.com"); ok {
		t.Error("a.b.docs.example.com matched *.docs.example.com; a wildcard that deep hands a subtree to whoever can name in it")
	}
}

func TestHostsAreComparedWithoutPortOrCase(t *testing.T) {
	config := mustParse(t, `version: 1
zones:
  dev:
    hosts: [Docs.Example.COM]
    access: public
`)
	for _, host := range []string{"docs.example.com", "docs.example.com.", "docs.example.com:8080"} {
		if _, ok := config.Zone(normaliseHost(host)); !ok {
			t.Errorf("%q matched no zone", host)
		}
	}
}

func TestAnUnknownKeyIsAnError(t *testing.T) {
	_, err := parse(t, `version: 1
zones:
  public:
    hosts: [docs.example.com]
    acces: public
`)
	if err == nil {
		t.Fatal("a misspelled key parsed; a zone with no access is exactly what must not be guessed at")
	}
}

func TestARestrictedZoneNeedsPrincipals(t *testing.T) {
	_, err := parse(t, `version: 1
zones:
  internet:
    hosts: [docs.example.com]
    access: restricted
`)
	if err == nil || !strings.Contains(err.Error(), "no principals") {
		t.Fatalf("error = %v, want a complaint about missing principals", err)
	}
}

func TestPrincipalsOnAPublicZoneAreAnError(t *testing.T) {
	_, err := parse(t, `version: 1
zones:
  intranet:
    hosts: [docs.internal]
    access: public
    principals: [reader]
principals:
  reader:
    tokens:
      - id: t1
        hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
`)
	if err == nil {
		t.Fatal("a public zone listing principals parsed, which reads as a policy that is not in force")
	}
}

func TestAHostCannotBelongToTwoZones(t *testing.T) {
	_, err := parse(t, `version: 1
zones:
  one:
    hosts: [docs.example.com]
    access: public
  two:
    hosts: [docs.example.com]
    access: off
`)
	if err == nil || !strings.Contains(err.Error(), "already claimed") {
		t.Fatalf("error = %v, want a complaint about a duplicate host", err)
	}
}

func TestAPrincipalNoZoneUsesIsAnError(t *testing.T) {
	_, err := parse(t, oneToken+`  spare:
    tokens:
      - id: t2
        hash: "sha256:1111111111111111111111111111111111111111111111111111111111111111"
`)
	if err == nil || !strings.Contains(err.Error(), "no zone lets it in") {
		t.Fatalf("error = %v, want a complaint about an unused principal", err)
	}
}

func TestAPlaintextPasswordIsRejected(t *testing.T) {
	_, err := parse(t, `version: 1
zones:
  internet:
    hosts: [docs.example.com]
    access: restricted
    principals: [reader]
principals:
  reader:
    password: "hunter2"
`)
	if err == nil || !strings.Contains(err.Error(), "unrecognised hash") {
		t.Fatalf("error = %v, want the plaintext password refused", err)
	}
}

func TestAnUnimplementedMethodIsRefusedAtStartup(t *testing.T) {
	_, err := parse(t, `version: 1
zones:
  internet:
    hosts: [docs.example.com]
    access: restricted
    method: oidc
    principals: [reader]
principals:
  reader:
    tokens:
      - id: t1
        hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
`)
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("error = %v, want the unimplemented method refused rather than silently ignored", err)
	}
}

func TestVersionMustBeOne(t *testing.T) {
	if _, err := parse(t, "version: 2\nzones:\n  a:\n    hosts: [x]\n    access: public\n"); err == nil {
		t.Fatal("version 2 parsed")
	}
}

func TestSummaryNamesEveryZone(t *testing.T) {
	summary := mustParse(t, oneToken).Summary()
	if !strings.Contains(summary, "internet restricted") {
		t.Errorf("Summary() = %q", summary)
	}
	var nilConfig *Config
	if got := nilConfig.Summary(); !strings.Contains(got, "public") {
		t.Errorf("nil Summary() = %q, want it to say everything is public", got)
	}
}

func TestBadHostPatterns(t *testing.T) {
	for _, host := range []string{"docs.*.example.com", "*docs.example.com", "*."} {
		_, err := parse(t, fmt.Sprintf("version: 1\nzones:\n  z:\n    hosts: [%q]\n    access: public\n", host))
		if err == nil {
			t.Errorf("%q parsed as a host pattern", host)
		}
	}
}

func TestAnEmptyZoneOrPrincipalIsAnError(t *testing.T) {
	// `intranet:` with nothing under it decodes to a nil pointer. It must be a
	// complaint, not a panic and not a zone with no policy.
	for _, yaml := range []string{
		"version: 1\nzones:\n  intranet:\n",
		"version: 1\nzones:\n  internet:\n    hosts: [docs.example.com]\n    access: restricted\n    principals: [reader]\nprincipals:\n  reader:\n",
	} {
		if _, err := parse(t, yaml); err == nil {
			t.Errorf("parsed an empty entry:\n%s", yaml)
		}
	}
}
