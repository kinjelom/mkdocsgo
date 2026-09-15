# Zones and authentication

Who may reach the documentation, decided by the address they arrived at.

Active whenever the project has an `mkdocsgo.yml`. Without that file the server
behaves as it always did: everything public, no credentials, no challenge.

## What a zone is, and what it is not

A **zone** is a policy for an address. Several addresses may share one, and
every address the server answers on belongs to exactly one.

A zone is **not** a filter over content. One process serves one set of pages,
so a restricted zone hides nothing from someone who knows a public zone's
address for the same deployment. That is deliberate: filtering at request time
would have to rewrite Material's `search_index.json`, the sitemap and the page
set together, and a half-filtered search index is a leak that looks like a
feature.

So zones separate **an intranet address from an internet one** for the same
pages. Documentation that must genuinely differ is a different build - a
different `exclude_docs`, a different image, a different deployment.

## The file

`mkdocsgo.yml` sits beside `mkdocs.yml` and is read at startup. `-config`
points elsewhere; a file named there and missing is an error, while the default
one missing is not.

```yaml
version: 1

zones:

  intranet:
    hosts:
      - docs.intranet.example.com
      - docs.apps.internal
      - localhost
      - 127.0.0.1
    access: public

  internet:
    hosts:
      - docs.example.com
      - "*.docs.example.com"
    access: restricted
    realm: "Example documentation"
    principals: [partner-a, ci-agent]

principals:

  partner-a:
    display: "Partner A"
    password: "$argon2id$v=19$m=19456,t=2,p=1$pZ05esn68w0jmDfS0vDkGg$O4dIN..."
    tokens:
      - id: 2026-09
        hash: "sha256:5d03d8b94d07f994306cd4b67f96dd82e0e9742739ba2f024723f9452cb8a540"
        expires: 2027-01-01

  ci-agent:
    tokens:
      - id: 2026-09
        hash: "sha256:3b7a9c..."
```

**Nothing in it is a secret.** A password is an Argon2id hash, a token is a
SHA-256 digest of a secret that was printed once. A leaked copy of this file is
not a leaked credential, which is why there is no encrypted variant to manage,
rotate, decrypt in a pipeline or lose.

| Key                             | Required        | Meaning                                                                                        |
|---------------------------------|-----------------|------------------------------------------------------------------------------------------------|
| `version`                       | yes             | `1`. An unknown version, or an unknown key anywhere, is a startup error                        |
| `trust_forwarded_host`          | no              | Believe `X-Forwarded-Host`. Off unless a proxy in front sets it and strips inbound ones        |
| `zones.<name>`                  | yes             | The name is the map key, so there is nothing to keep in step                                   |
| `zones.*.hosts`                 | yes             | Addresses; port and case are ignored, IDN hosts are written as punycode                        |
| `zones.*.access`                | yes             | `public`, `restricted`, or `off` - 403 to everything, retiring an address without touching DNS |
| `zones.*.realm`                 | no              | Shown in the browser's credentials prompt; defaults to the zone name                           |
| `zones.*.method`                | no              | `static` (the default and, today, the only one). The seam for `oidc`                           |
| `zones.*.principals`            | when restricted | Who may in                                                                                     |
| `principals.<name>`             | -               | The name is the Basic auth login                                                               |
| `principals.*.display`          | no              | For humans reading the file                                                                    |
| `principals.*.password`         | no              | `$argon2id$...` or `$2y$...`. Absent means no browser access                                   |
| `principals.*.tokens[].id`      | yes             | A label for the access log and for rotation; the secret never appears                          |
| `principals.*.tokens[].hash`    | yes             | `sha256:<hex>`                                                                                 |
| `principals.*.tokens[].expires` | no              | `YYYY-MM-DD`, the last day the token works                                                     |

### It fails at startup, not at request time

A restricted zone with no principals, a principal a zone names but nobody
defined, a principal nobody lets in, a plaintext password where a hash belongs,
one host claimed by two zones, an unknown key, `method: oidc` - each stops the
server from starting.

The alternative is a zone that silently has no way in, or worse, one whose
policy is not the one written down. A documentation server that refuses to
start is a deployment that rolls back; a documentation server with the wrong
policy is not noticed.

## Matching an address

The most specific pattern wins, whatever order the file lists zones in:

1. an exact host - `docs.example.com`
2. a one-label wildcard, longest first - `*.docs.example.com` before `*.example.com`
3. a domain suffix, longest first - `**.cfp1.i6e.in` before `**.in`
4. the catch-all `"*"`, if one is written

| Pattern | Covers | Does not cover |
|---|---|---|
| `docs.example.com` | that host | anything else |
| `*.docs.example.com` | `a.docs.example.com` | `a.b.docs.example.com` |
| `**.in` | `a.in`, `a.b.c.in` | `in`, `a.internal` |
| `*` | everything left | - |

`*` stands for **exactly one label**, because a wildcard that swallowed any
depth would hand a subtree to whoever can create a name in it. `**` is that
wider match, spelled differently so it cannot be written by accident: it is for
a policy stated in terms of a domain - "every internal foundation is internal"
- which is the case where a route nobody has created yet should already be
covered by the right zone.

**An address no zone claims gets 403.** Fail closed: a stale DNS record
pointing at the application is far likelier than a zone somebody forgot.

`Host` is taken from the request. `X-Forwarded-Host` is ignored unless
`trust_forwarded_host` is on - the Cloud Foundry router and every Ingress
controller pass the real `Host` through, and trusting a header the client can
set would let the client choose its own policy.

`/healthz` is outside every zone. A platform health check arrives at the
container's address rather than at a route, so a zone would turn every probe
into a 403 and the application would never come up.

## Credentials

The server mints them, because a hash is not something to assemble by hand:

```console
$ mkdocsgo -new-token
token (copy it now, it cannot be recovered):
  mkd_IVkUZxLOnz1G6Js4XNSOYahapTbfOhnM9xvCegPV1Ag

paste into mkdocsgo.yml, under the principal it belongs to:
    tokens:
      - id: 2026-09
        hash: "sha256:5d03d8b94d07f994306cd4b67f96dd82e0e9742739ba2f024723f9452cb8a540"

$ mkdocsgo -hash-password
password:
again   :

paste into mkdocsgo.yml, under the principal it belongs to:
    password: "$argon2id$v=19$m=19456,t=2,p=1$pZ05esn68w0jmDfS0vDkGg$O4dIN..."
```

`-hash-password` reads stdin when there is no terminal, so a script can pipe
one in.

**Rotating a token** is adding a second entry and removing the first once every
client has moved. Both work at once, so rotation needs no outage. The `id` is
what the access log prints, which is how you tell whether the old one is still
in use before deleting it.

**Why two different hashes.** A password is chosen by a person and therefore
guessable, so it gets Argon2id - 19 MiB and two passes, the OWASP parameters
that fit a container sized for a documentation server rather than the 64 MiB
ones that would not. A token is 256 bits of randomness, so there is nothing for
a slow hash to defend, and paying 19 MiB per request would hand an agent's
retry loop a way to exhaust the process. Bcrypt hashes are accepted too, so an
existing `htpasswd -B` file can be moved over as it is.

## What a restricted zone changes

|                       |                                                                                                                                                                                                                                                    |
|-----------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Site, no credentials  | `401` + `WWW-Authenticate: Basic realm="…", charset="UTF-8"`                                                                                                                                                                                       |
| MCP, no credentials   | `401` + `WWW-Authenticate: Bearer realm="…", resource_metadata="…"`                                                                                                                                                                                |
| Every served response | `Cache-Control` downgraded from `public` to `private`, and `X-Robots-Tag: noindex, nofollow`                                                                                                                                                       |
| Access log            | ` zone=internet principal=partner-a/2026-09`; the `Authorization` header is never logged                                                                                                                                                           |
| A rejected credential | Costs 300 ms before the 401, which slows guessing and flattens the timing difference between a wrong password and an unknown principal. A request carrying no credentials at all - every browser visit begins that way - is challenged immediately |

The cache downgrade matters more than it looks. The policy in
[SERVING.md](./SERVING.md) marks fingerprinted assets `public, immutable` -
exactly what a shared cache is entitled to keep and hand to the next person.
The freshness is kept; only the right to store it in a shared cache is taken
away.

## The two surfaces

**A browser** gets HTTP Basic. There is no logout - that is Basic auth, not a
choice made here - which is one of the reasons the browser half is expected to
move to Keycloak first.

```bash
curl -u partner-a https://docs.example.com/
```

**An agent** presents a bearer token, which is what the MCP specification asks
for and what every MCP client can send:

```json
{ "mcpServers": { "docs": {
  "type": "http",
  "url": "https://docs.example.com/mcp",
  "headers": { "Authorization": "Bearer mkd_IVkUZxLOnz1G6Js4XNSOYahapTbfOhnM9xvCegPV1Ag" }
}}}
```

There is no `X-API-Key` fallback. A custom header is in no specification and no
client's discovery path, and a second way in is a second way to get the policy
wrong.

**stdio has no zones.** There is no `Host` to match and the client is a process
the user started themselves. The server says so at startup rather than letting
anyone believe a policy is in force.

## Protected resource metadata

A restricted zone publishes [RFC 9728](https://datatracker.ietf.org/doc/html/rfc9728)
metadata at `/.well-known/oauth-protected-resource/mcp`, and at the bare
`/.well-known/oauth-protected-resource` for clients that ask there. Both are
outside the zone policy: a client reads them in order to learn how to
authenticate, so requiring authentication would close the loop.

```json
{
  "resource": "https://docs.example.com/mcp",
  "resource_name": "Example documentation",
  "bearer_methods_supported": ["header"]
}
```

No `authorization_servers`: with `method: static` there is no OAuth server to
name, and RFC 9728 makes the field optional for exactly this case. The point of
publishing it anyway is the day it stops being optional - a client that already
follows the `resource_metadata` pointer needs no change when the field names
Keycloak.

## Testing it locally

Add the loopback addresses to a public zone, as the example above does. Two
things otherwise get in the way:

- an address no zone claims is 403, and `Host: 127.0.0.1` is such an address
  unless you say so - which is also why `-mcp-probe` against a restricted zone
  needs credentials it has no way to send;
- the MCP SDK refuses a request that reaches a **loopback** listener carrying a
  non-loopback `Host`. That is its own DNS-rebinding defence, unrelated to
  zones, and it means `curl -H 'Host: docs.example.com' http://127.0.0.1:8080/mcp`
  is refused while the same request to the machine's real address is not.

## The road to Keycloak

`method` is the seam. When a zone becomes

```yaml
    method: oidc
    oidc:
      issuer: https://kc.example.com/realms/docs
      audience: https://docs.example.com
      required_scopes: [docs.read]
```

the bearer half changes verifier - a JWT checked against the realm's JWKS, with
the audience required to name this server, as RFC 8707 asks - and nothing else
in the file moves. `principals` shrinks to the machine tokens, or disappears.

The browser half is the larger piece of work: an authorization code flow, a
session cookie, a signing key, a logout. The cheap route there is a `forwarded`
method - oauth2-proxy in front doing OIDC and passing an identity header that
this server maps to a principal, trusted only on a network where nothing else
can set it. Neither is implemented today, and both are refused at startup
rather than ignored.

## Limits

- **One policy per zone**, covering both surfaces. A zone that is public for
  browsers and restricted for agents needs two keys that do not exist yet.
- **No groups, no roles, no per-path rules.** A principal is either in a zone
  or not.
- **No rate limiting** beyond the fixed delay on a rejected credential, and no
  lockout. Anything stronger belongs in front, where the request volume is
  already visible.
- **No TLS.** Unchanged: something terminates it in front, in every deployment
  this runs in. Basic auth over plain HTTP is a password in clear text, so a
  restricted zone without TLS in front of it is a mistake this server cannot
  detect.
