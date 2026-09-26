# Zones and authentication

Who may reach the documentation, decided by the address they arrived at.

Active whenever the project has an `mkdocsgo.yml`. Without that file the server
behaves as it always did: everything public, no credentials, no challenge.

A restricted zone asks a browser for a password and an agent for a bearer
token. The agent is either handed one, or - with `oauth: true` - signs in as a
principal through an OAuth authorization server built into the binary, which
is how **claude.ai** connects. See
[Signing in from claude.ai](#signing-in-from-claudeai-oauth).

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
    oauth: true            # claude.ai signs in as partner-a; ci-agent keeps its token

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
rotate, decrypt in a pipeline or lose. The one secret OAuth needs is a key in
the environment, `MKDOCSGO_OAUTH_KEY`, and never in this file.

| Key                             | Required        | Meaning                                                                                        |
|---------------------------------|-----------------|------------------------------------------------------------------------------------------------|
| `version`                       | yes             | `1`. An unknown version, or an unknown key anywhere, is a startup error                        |
| `trust_forwarded_host`          | no              | Believe `X-Forwarded-Host`. Off unless a proxy in front sets it and strips inbound ones        |
| `oauth`                         | no              | Settings for the built-in authorization server. Every key is optional, and so is the block     |
| `oauth.redirect_uris`           | no              | Where a sign-in may send its code. Defaults to Claude's; a list here replaces the defaults     |
| `oauth.access_token_ttl`        | no              | How long an access token lasts; `1h` by default. A Go duration: `15m`, `2h`                    |
| `oauth.refresh_token_ttl`       | no              | How long a sign-in lasts before the person signs in again; `720h` (30 days) by default         |
| `zones.<name>`                  | yes             | The name is the map key, so there is nothing to keep in step                                   |
| `zones.*.hosts`                 | yes             | Addresses; port and case are ignored, IDN hosts are written as punycode                        |
| `zones.*.access`                | yes             | `public`, `restricted`, or `off` - 403 to everything, retiring an address without touching DNS |
| `zones.*.realm`                 | no              | Shown in the browser's credentials prompt; defaults to the zone name                           |
| `zones.*.method`                | no              | `static` (the default and, today, the only one). The seam for `oidc`                           |
| `zones.*.principals`            | when restricted | Who may in                                                                                     |
| `zones.*.oauth`                 | no              | `true` lets an MCP client sign in as one of the zone's principals, with its password           |
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
server from starting. So do `oauth: true` on a zone nobody signs in to (a public
one, or one whose principals have no password), `oauth:` settings no zone uses,
and a zone with `oauth: true` served over HTTP without `MKDOCSGO_OAUTH_KEY`.

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
| MCP, a refused token  | The same, with `error="invalid_token"` - what tells an OAuth client to refresh rather than start over                                                                                                                                              |
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
for. Either it signs in and gets one - see the next section, which is the only
way for claude.ai - or it is handed one, which every client that can set a
header can send:

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

## Signing in from claude.ai: OAuth

claude.ai, Claude Desktop and Claude mobile add a remote MCP server by its URL
and nothing else - there is nowhere to paste a token. When the server answers
`401`, they sign in, the way the MCP specification describes. A zone with
`oauth: true` lets them, through an OAuth 2.1 authorization server built into
this binary. The person signing in is one of the zone's principals, with the
password already in the file.

```yaml
zones:
  internet:
    hosts: [docs.example.com]
    access: restricted
    principals: [partner-a, ci-agent]
    oauth: true
```

...and one key in the environment, the same on every instance:

```bash
export MKDOCSGO_OAUTH_KEY="$(openssl rand -base64 32)"
```

That is all. Static tokens keep working beside it, so `ci-agent` changes
nothing; a principal with no password - a machine - cannot sign in, as it cannot
use Basic either.

### Connecting Claude

**claude.ai**, **Desktop** and **mobile**: *Customize → Connectors → Add custom
connector*, the URL `https://docs.example.com/mcp`, nothing under the advanced
settings. *Connect* opens this server's sign-in page; the principal's name and
password bring you back to Claude, connected. On a Team or Enterprise plan an
Owner adds the connector under *Organization settings → Connectors*, and each
member then connects as their own principal.

**Claude Code** signs in the same way, through a loopback redirect on a port
it picks:

```bash
claude mcp add --transport http docs https://docs.example.com/mcp
# then /mcp inside Claude Code, and Authenticate
```

### What happens

```mermaid
sequenceDiagram
    participant C as Claude
    participant B as Browser
    participant S as mkdocsgo

    C->>S: POST /mcp
    S-->>C: 401, WWW-Authenticate: Bearer resource_metadata=...
    C->>S: GET /.well-known/oauth-protected-resource/mcp
    S-->>C: authorization_servers: [https://docs.example.com]
    C->>S: GET /.well-known/oauth-authorization-server
    C->>S: POST /oauth/register
    S-->>C: client_id
    C->>B: open /oauth/authorize?...&code_challenge=...
    B->>S: GET, then POST name and password
    S-->>B: 303 to https://claude.ai/api/mcp/auth_callback?code=...
    C->>S: POST /oauth/token - code + PKCE verifier
    S-->>C: access token (1 h) + refresh token
    C->>S: POST /mcp, Authorization: Bearer mkda_...
```

| Endpoint                                  | What                                                                  |
|-------------------------------------------|-----------------------------------------------------------------------|
| `/.well-known/oauth-authorization-server` | RFC 8414 metadata. The issuer is the origin the request arrived at    |
| `/oauth/register`                         | RFC 7591 dynamic client registration                                  |
| `/oauth/authorize`                        | The sign-in page, and the authorization code it hands back            |
| `/oauth/token`                            | A code for tokens, PKCE with S256 required; a refresh token for more  |

All four sit outside the zone policy, like the resource metadata: a client comes
here because it has no credentials yet. On a host whose zone has no
`oauth: true`, all four are 404.

### Nothing is stored

A client registration, an authorization code, an access token and a refresh
token are each a small JSON document with an HMAC-SHA256 over it, under
`MKDOCSGO_OAUTH_KEY`. So registering writes nothing - Claude registers anew on
every fresh connection - any instance with the key accepts what another one
issued, and a restart signs nobody out. No database, no volume, no session
affinity.

| Issued        | Lasts                                                                  | Bound to                                                         |
|---------------|------------------------------------------------------------------------|------------------------------------------------------------------|
| Access token  | `access_token_ttl`, an hour by default                                 | the host it was issued on, the zone, the principal, its password |
| Refresh token | until `refresh_token_ttl` after sign-in; refreshing does not extend it | the same, and the client                                         |
| Code          | a minute, redeemable once per instance                                 | the client, its redirect URI and its PKCE challenge              |

The access log names who signed in, and on what:
` zone=internet principal=partner-a/oauth`.

### Signing someone out

Nothing is stored, so no single token can be withdrawn. Three levers instead,
coarsest last:

1. **Take the principal out of the zone**, and restart.
2. **Change its password**, and restart. Every client that password signed in
   is refused at its next request, and its refresh fails with `invalid_grant` -
   which is what makes Claude ask for a new sign-in.
3. **Change `MKDOCSGO_OAUTH_KEY`.** Everyone, everywhere, signs in again.

Why it is built this way, and what else was considered, is in
[ADR 0002](./docs/adr/0002-built-in-oauth-for-mcp-clients.md).

### Where a code may be sent

Anybody may register a client - that is what dynamic registration is - so what
a registration may ask for is the policy. Without it, anyone could register
their own address and send a principal a sign-in link that ends there.
`oauth.redirect_uris` is that allowlist:

| Default                                                  | For                                         |
|----------------------------------------------------------|---------------------------------------------|
| `https://claude.ai/api/mcp/auth_callback`                | claude.ai, Claude Desktop, Claude mobile    |
| `https://claude.com/api/mcp/auth_callback`               | the same, should Anthropic move it          |
| `http://localhost/callback`, `http://127.0.0.1/callback` | Claude Code, on whatever port it picked     |

A loopback entry ignores the port, as RFC 8252 asks, and written without a path
admits any path. Anything else is compared exactly, and must be `https`. A list
in the file replaces the defaults rather than adding to them, so to add another
client, write Claude's entries next to it:

```yaml
oauth:
  redirect_uris:
    - https://claude.ai/api/mcp/auth_callback
    - http://localhost/callback
    - http://127.0.0.1/callback
    - http://localhost            # MCP Inspector, any port and path
```

The sign-in page leads with where the person will be sent afterwards, because
that is what the allowlist vouches for; the name a client registers under is
its own choice.

### What it asks of the deployment

- **claude.ai must reach the address from the internet.** It connects from
  Anthropic's infrastructure (`160.79.104.0/21`), and refuses a name that
  resolves to a private address. An intranet-only route cannot be a claude.ai
  connector, whatever this server does; Claude Code, running on the user's
  machine, can use one.
- **HTTPS in front**, as for Basic: a code or a token in clear text is a
  credential in clear text. The issuer is `https://` plus the host, or `http://`
  for a loopback address, or whatever `X-Forwarded-Proto` says when
  `trust_forwarded_host` is on.
- **One key on every instance.** A code issued by one is redeemed at whichever
  the load balancer picks next. The server refuses to start when a zone asks for
  OAuth, `/mcp` is served over HTTP and the key is missing or shorter than 32
  characters. A `-mode site` deployment or a stdio process with the same file
  does not need it.
- **A sign-in costs one password verification**, the same 19 MiB as a Basic
  login; checking a token costs an HMAC.

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
  "bearer_methods_supported": ["header"],
  "authorization_servers": ["https://docs.example.com"]
}
```

`authorization_servers` is there when the zone has `oauth: true`, and names
this origin, where the built-in authorization server answers. Without it the
field is absent: tokens are issued out of band, and RFC 9728 makes the field
optional for exactly this case. `resource` is the MCP URL exactly as a person
types it into Claude, which is what Claude checks it against.

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

OAuth can be tried the same way, with `localhost` in a restricted zone that has
`oauth: true`: the issuer is then `http://localhost:<port>`, and Claude Code
signs in against it as it would against the real address. claude.ai cannot -
it connects from Anthropic's network, not from yours.

## The road to Keycloak

OAuth for agents exists now, with the principals in the file as its users.
Keycloak is still the destination when the users should come from an identity
provider instead, and `method` is still the seam. When a zone becomes

```yaml
    method: oidc
    oidc:
      issuer: https://kc.example.com/realms/docs
      audience: https://docs.example.com
      required_scopes: [docs.read]
```

the bearer half changes verifier - a JWT checked against the realm's JWKS, with
the audience required to name this server, as RFC 8707 asks - and
`authorization_servers` names the realm instead of this origin. A client that
follows the pointer, claude.ai included, changes nothing. `principals` shrinks
to the machine tokens, or disappears.

The browser half is the larger piece of work: an authorization code flow, a
session cookie, a signing key, a logout. The cheap route there is a `forwarded`
method - oauth2-proxy in front doing OIDC and passing an identity header that
this server maps to a principal, trusted only on a network where nothing else
can set it. Neither is implemented today, and both are refused at startup
rather than ignored.

## Limits

- **OAuth is for `/mcp` only.** A browser still gets HTTP Basic.
- **No token can be withdrawn on its own**, and refresh tokens are not
  rotated: nothing is stored to withdraw or rotate against. See
  [Signing someone out](#signing-someone-out).
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
