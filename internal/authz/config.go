package authz

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultFileName is the server configuration a project may put beside its
// mkdocs.yml. Its absence is not an error: a project without one serves
// everything publicly, which is what every deployment did before zones existed.
const DefaultFileName = "mkdocsgo.yml"

// Access is what a zone grants.
type Access string

const (
	// AccessPublic serves the zone to anyone, as before.
	AccessPublic Access = "public"
	// AccessRestricted requires credentials on both surfaces.
	AccessRestricted Access = "restricted"
	// AccessOff answers 403 to everything. It takes an address out of service
	// without touching DNS, the router or the manifest.
	AccessOff Access = "off"
)

// MethodStatic verifies credentials against the hashes in this file. The field
// exists so that `method: oidc` can arrive later without the rest of the file
// changing shape.
const MethodStatic = "static"

// Config is a parsed mkdocsgo.yml.
type Config struct {
	Version            int                   `yaml:"version"`
	TrustForwardedHost bool                  `yaml:"trust_forwarded_host"`
	Zones              map[string]*Zone      `yaml:"zones"`
	Principals         map[string]*Principal `yaml:"principals"`

	// Path is where this came from, for error messages and the startup log.
	Path string `yaml:"-"`

	exact     map[string]*Zone
	wildcards []*Zone
	suffixes  []*Zone
	catchAll  *Zone
}

// Zone is one access policy and the addresses it applies to.
type Zone struct {
	Hosts      []string `yaml:"hosts"`
	Access     Access   `yaml:"access"`
	Realm      string   `yaml:"realm"`
	Method     string   `yaml:"method"`
	Principals []string `yaml:"principals"`

	Name    string `yaml:"-"`
	members []*Principal
	// patterns holds this zone's one-label wildcards as the suffix they match
	// on: "*.docs.example.com" is held as ".docs.example.com".
	patterns []string
	// suffixes holds its any-depth wildcards the same way: "**.in" as ".in".
	suffixes []string
}

// Principal is one named identity: a person with a password, a machine with a
// token, or both.
type Principal struct {
	Display  string   `yaml:"display"`
	Password string   `yaml:"password"`
	Tokens   []*Token `yaml:"tokens"`

	Name string `yaml:"-"`
}

// Token is one bearer credential. The secret itself was printed once by
// `mkdocsgo -new-token`; what is stored is its SHA-256 digest.
type Token struct {
	ID      string `yaml:"id"`
	Hash    string `yaml:"hash"`
	Expires string `yaml:"expires"`

	expires time.Time
}

// Restricted reports whether the zone needs credentials.
func (z *Zone) Restricted() bool { return z.Access == AccessRestricted }

// Load reads the configuration at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	config, err := Parse(raw, path)
	if err != nil {
		return nil, err
	}
	return config, nil
}

// Parse decodes and validates a configuration.
//
// Unknown keys are an error rather than a warning. A misspelled `principals:`
// under a restricted zone would otherwise leave that zone with no members and
// no way in - or, worse in a later revision, with a policy nobody intended.
func Parse(raw []byte, path string) (*Config, error) {
	config := &Config{Path: path}

	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(config); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
}

func (c *Config) validate() error {
	if c.Version != 1 {
		return fmt.Errorf("version must be 1, got %d", c.Version)
	}
	if len(c.Zones) == 0 {
		return fmt.Errorf("no zones defined - remove the file instead, which serves everything publicly")
	}

	for name, principal := range c.Principals {
		// `demo:` with nothing under it decodes to a nil pointer, and reads as
		// a principal with no way to authenticate rather than as a mistake.
		if principal == nil {
			return fmt.Errorf("principals.%s: nothing under it", name)
		}
		principal.Name = name
		if err := principal.validate(); err != nil {
			return fmt.Errorf("principals.%s: %w", name, err)
		}
	}

	c.exact = map[string]*Zone{}
	hostOwner := map[string]string{}
	used := map[string]bool{}

	// Sorted, so two runs over the same file report the same first error.
	names := make([]string, 0, len(c.Zones))
	for name := range c.Zones {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		zone := c.Zones[name]
		if zone == nil {
			return fmt.Errorf("zones.%s: nothing under it", name)
		}
		zone.Name = name
		if err := c.validateZone(zone, hostOwner, used); err != nil {
			return fmt.Errorf("zones.%s: %w", name, err)
		}
	}

	// A principal nobody lets in is a typo far more often than it is an
	// intention, and the cost of the alternative is a zone quietly missing a
	// member.
	for _, name := range sortedKeys(c.Principals) {
		if !used[name] {
			return fmt.Errorf("principals.%s: declared but no zone lets it in", name)
		}
	}

	// Longest suffix first: *.docs.example.com must win over *.example.com
	// whatever order the file happens to list the zones in, and **.i6e.in over
	// **.in.
	sort.SliceStable(c.wildcards, func(i, j int) bool {
		return longest(c.wildcards[i].patterns) > longest(c.wildcards[j].patterns)
	})
	sort.SliceStable(c.suffixes, func(i, j int) bool {
		return longest(c.suffixes[i].suffixes) > longest(c.suffixes[j].suffixes)
	})
	return nil
}

func (c *Config) validateZone(zone *Zone, hostOwner map[string]string, used map[string]bool) error {
	switch zone.Access {
	case AccessPublic, AccessRestricted, AccessOff:
	case "":
		return fmt.Errorf("access is required: public, restricted or off")
	default:
		return fmt.Errorf("access %q is not public, restricted or off", zone.Access)
	}

	if len(zone.Hosts) == 0 {
		return fmt.Errorf("hosts is required - a zone nothing reaches has no effect")
	}
	for _, host := range zone.Hosts {
		// Checked before normalising, not after: normalisation strips a
		// trailing dot, and "*." would otherwise arrive here as "*" and
		// quietly become a catch-all nobody wrote.
		if err := validHostPattern(strings.TrimSpace(strings.ToLower(host))); err != nil {
			return fmt.Errorf("hosts: %q: %w", host, err)
		}
		pattern := normaliseHost(host)
		if owner, taken := hostOwner[pattern]; taken {
			return fmt.Errorf("hosts: %q is already claimed by zone %q", host, owner)
		}
		hostOwner[pattern] = zone.Name

		switch {
		case pattern == "*":
			if c.catchAll != nil {
				return fmt.Errorf(`hosts: a second catch-all "*" - zone %q already has one`, c.catchAll.Name)
			}
			c.catchAll = zone
		case strings.HasPrefix(pattern, "**."):
			zone.suffixes = append(zone.suffixes, pattern[2:]) // ".in"
			if !containsZone(c.suffixes, zone) {
				c.suffixes = append(c.suffixes, zone)
			}
		case strings.HasPrefix(pattern, "*."):
			zone.patterns = append(zone.patterns, pattern[1:]) // ".docs.example.com"
			if !containsZone(c.wildcards, zone) {
				c.wildcards = append(c.wildcards, zone)
			}
		default:
			c.exact[pattern] = zone
		}
	}

	if zone.Method == "" {
		zone.Method = MethodStatic
	}
	if zone.Method != MethodStatic {
		return fmt.Errorf("method %q is not implemented; only %q verifies credentials today - see AUTH.md",
			zone.Method, MethodStatic)
	}

	if !zone.Restricted() {
		if len(zone.Principals) > 0 {
			return fmt.Errorf("principals are listed but access is %q, so nobody is ever asked for them", zone.Access)
		}
		return nil
	}

	if len(zone.Principals) == 0 {
		return fmt.Errorf("access is restricted but no principals are listed, so nobody could ever get in")
	}
	for _, name := range zone.Principals {
		principal, ok := c.Principals[name]
		if !ok {
			return fmt.Errorf("principals: %q is not defined", name)
		}
		zone.members = append(zone.members, principal)
		used[name] = true
	}
	if zone.Realm == "" {
		zone.Realm = zone.Name
	}
	return nil
}

func (p *Principal) validate() error {
	if p.Password == "" && len(p.Tokens) == 0 {
		return fmt.Errorf("neither a password nor a token, so it can never authenticate")
	}
	if p.Password != "" {
		if err := checkPasswordHash(p.Password); err != nil {
			return fmt.Errorf("password: %w", err)
		}
	}
	seen := map[string]bool{}
	for _, token := range p.Tokens {
		if token == nil {
			return fmt.Errorf("tokens: an empty entry")
		}
		if token.ID == "" {
			return fmt.Errorf("tokens: id is required - it is what the access log and a rotation refer to")
		}
		if seen[token.ID] {
			return fmt.Errorf("tokens: duplicate id %q", token.ID)
		}
		seen[token.ID] = true
		if err := checkTokenHash(token.Hash); err != nil {
			return fmt.Errorf("tokens.%s: hash: %w", token.ID, err)
		}
		if token.Expires != "" {
			day, err := time.Parse(time.DateOnly, token.Expires)
			if err != nil {
				return fmt.Errorf("tokens.%s: expires: %q is not a YYYY-MM-DD date", token.ID, token.Expires)
			}
			// Through the end of the named day, UTC: "expires 2027-01-01"
			// reads as a last day of validity, not as a deadline at midnight
			// the night before.
			token.expires = day.AddDate(0, 0, 1)
		}
	}
	return nil
}

// Zone resolves a host to its zone. The most specific pattern wins, whatever
// order the file lists them in: an exact host, then the longest one-label
// wildcard, then the longest any-depth suffix, then a catch-all if one was
// declared.
func (c *Config) Zone(host string) (*Zone, bool) {
	if c == nil {
		return nil, false
	}
	if zone, ok := c.exact[host]; ok {
		return zone, true
	}
	for _, zone := range c.wildcards {
		for _, suffix := range zone.patterns {
			if matchesWildcard(host, suffix) {
				return zone, true
			}
		}
	}
	for _, zone := range c.suffixes {
		for _, suffix := range zone.suffixes {
			if matchesSuffix(host, suffix) {
				return zone, true
			}
		}
	}
	if c.catchAll != nil {
		return c.catchAll, true
	}
	return nil, false
}

// Summary is the one line the server logs at startup.
func (c *Config) Summary() string {
	if c == nil {
		return "no zones: everything is public"
	}
	parts := make([]string, 0, len(c.Zones))
	for _, name := range sortedKeys(c.Zones) {
		zone := c.Zones[name]
		part := fmt.Sprintf("%s %s", name, zone.Access)
		if zone.Restricted() {
			part += fmt.Sprintf(" (%d principals)", len(zone.members))
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// matchesWildcard reports whether host is one label below suffix, which starts
// with a dot. `*` stands for exactly one label: *.docs.example.com covers
// a.docs.example.com and not a.b.docs.example.com, because a wildcard that
// swallowed any depth would hand a whole subtree to whoever can create a name
// in it.
func matchesWildcard(host, suffix string) bool {
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	label := host[:len(host)-len(suffix)]
	return label != "" && !strings.Contains(label, ".")
}

// matchesSuffix reports whether host sits anywhere under suffix, which starts
// with a dot. `**.in` covers a.in and a.b.c.in alike: that is what makes a
// policy such as "every internal foundation" survive a route nobody has
// created yet, and why it is spelled differently from the one-label `*.`.
func matchesSuffix(host, suffix string) bool {
	return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
}

func longest(patterns []string) int {
	longest := 0
	for _, pattern := range patterns {
		if len(pattern) > longest {
			longest = len(pattern)
		}
	}
	return longest
}

func containsZone(zones []*Zone, zone *Zone) bool {
	for _, candidate := range zones {
		if candidate == zone {
			return true
		}
	}
	return false
}

// normaliseHost reduces a Host header or a configured pattern to what they are
// compared as: lower case, no port, no trailing dot, no brackets around an
// IPv6 literal.
//
// Internationalised names are compared as the A-labels they arrive as - a Host
// header carries punycode - so a configuration file must spell them that way
// too. That is one line in AUTH.md and no Unicode normalisation here.
func normaliseHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return host
}

func validHostPattern(pattern string) error {
	switch {
	case pattern == "":
		return fmt.Errorf("empty")
	case pattern == "*":
		return nil
	case strings.HasPrefix(pattern, "**."):
		if strings.Contains(pattern[3:], "*") {
			return fmt.Errorf("only one wildcard, and only as the first label")
		}
		if pattern[3:] == "" {
			return fmt.Errorf("a wildcard needs a domain under it")
		}
		return nil
	case strings.HasPrefix(pattern, "*."):
		if strings.Contains(pattern[2:], "*") {
			return fmt.Errorf("only one wildcard, and only as the first label")
		}
		if pattern[2:] == "" {
			return fmt.Errorf("a wildcard needs a domain under it")
		}
		return nil
	case strings.Contains(pattern, "*"):
		return fmt.Errorf("a wildcard is only allowed as the first label, as in *.docs.example.com")
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
