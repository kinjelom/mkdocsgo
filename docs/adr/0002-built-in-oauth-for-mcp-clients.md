# 2. A built-in OAuth authorization server for MCP clients, with nothing stored

- **Status:** accepted
- **Date:** 2026-09-26
- **Applies to:** mkdocsgo after 0.3.0
- **Builds on:** [0001](./0001-zone-based-authentication.md)

## Context

[ADR 0001](./0001-zone-based-authentication.md) gave a restricted zone two ways
in: HTTP Basic for a browser, a static bearer token for an agent. The token
works for every client that lets you set a header - Claude Code, CI, the MCP
Inspector.

It does not work for **claude.ai**, which is where most people meet Claude.
claude.ai, Claude Desktop and Claude mobile add a remote MCP server by its URL
and nothing else. Their answer to a `401` is to sign in, the way the MCP
specification describes: follow `resource_metadata` to RFC 9728 metadata, find
an authorization server in it, register a client there (RFC 7591, or a Client
ID Metadata Document), send the user through an authorization code flow with
PKCE, and refresh the token when it runs out. A request header exists in beta,
for a limited set of organisations, and even there it is one credential shared
by everyone in the organisation - which is the opposite of what a principal is.

0001 left `authorization_servers` out of the metadata and named Keycloak, via
`method: oidc`, as the way it would one day be filled in. That is still a good
destination, but it makes "use the documentation from claude.ai" wait on an
identity provider, a realm, a client registration policy that admits
`claude.ai`, an audience mapper for RFC 8707, and a second thing to deploy
next to a server whose point is that it is one thing.

## Decision

**An OAuth 2.1 authorization server inside the binary, turned on per zone
with `oauth: true`.** It serves RFC 8414 metadata, dynamic client
registration, the authorization code flow with PKCE (S256 only) and refresh.
The protected resource metadata of such a zone names it in
`authorization_servers`. The format is in [AUTH.md](../../AUTH.md).

**The users are the zone's principals, signing in with the password already
in the file.** No second list of users, no second place to revoke one. A
principal without a password - a CI token - cannot sign in, as it cannot use
Basic.

**Nothing is stored.** A client registration, an authorization code, an
access token and a refresh token are each a small JSON document with an
HMAC-SHA256 over it, under a key from `MKDOCSGO_OAUTH_KEY`. Registration writes
nothing - which matters, because Claude registers a new client on every fresh
connection. Any instance holding the key accepts what another issued, so the
deployment stays stateless: no database, no volume, no session affinity, and a
restart signs nobody out.

**The key lives in the environment, not in `mkdocsgo.yml`.** It is the first
secret this server has, and the file's whole design is that it is not one.
Startup fails when a zone asks for OAuth, `/mcp` is served over HTTP and the
key is missing or shorter than 32 characters.

**Where codes may go is the policy.** Anyone may register a client; that is
what dynamic registration is. So a registration may only name redirect URIs
from an allowlist, `oauth.redirect_uris`, which defaults to Claude's callback
and to Claude Code's loopback redirect on any port. Without it, anybody could
register their own address and send a principal a sign-in link that ends
there.

**Grants are bound to where and to whom.** An access token names the host it
was issued on (RFC 8707's audience, without a JWT), the zone, the principal
and a fingerprint of the principal's password hash. It is checked against the
file on every request, so taking a principal out of a zone, or changing its
password, and restarting signs out every client it signed in.

## Consequences

**Gained.** claude.ai, Claude Desktop, Claude mobile and Claude Code connect
by URL, and each person connects as their own principal. Nothing new to deploy
and nothing new to store; the policy is still reviewed in the pull request
that changes the pages. Static tokens keep working beside it, so CI changes
nothing.

**Paid.**

- **A secret now exists.** One key, the same on every instance, delivered by
  the platform's secret mechanism. Losing it costs a sign-in for everyone;
  leaking it lets its holder mint tokens for any principal of any OAuth zone.
  That is the risk 0001 avoided by storing hashes, accepted here in exchange
  for statelessness.
- **No token can be withdrawn on its own.** Revocation is per principal
  (remove it, or change its password, then restart), or for everyone (change
  the key). Until then an access token lasts its hour.
- **Refresh tokens are not rotated.** OAuth 2.1 asks a server to rotate a
  public client's refresh token or to bind it to a key; rotation that the
  server cannot enforce - it would need to remember every spent token - would
  only look like it. What stands in for it: the token is bound to its client,
  host, zone and password, and the sign-in behind it ends at a fixed time
  (30 days by default) that refreshing does not extend. The clients the
  allowlist admits keep it server-side (claude.ai) or in the user's keychain
  (Claude Code).
- **Codes are single-use per instance only.** Across instances a replay is
  stopped by PKCE and a one-minute lifetime: a code is useless without the
  verifier, which never leaves the client.
- **A login page.** The binary now renders a form, with the obligations that
  come with one: no framing, no caching, the same delay on a wrong password as
  Basic, and a page that names where the person is being sent.
- **claude.ai must reach the address from the internet.** It connects from
  Anthropic's infrastructure and refuses names that resolve to private
  addresses. An intranet-only route cannot be a claude.ai connector, whatever
  this server does.

**Reversible.** Removing `oauth: true` removes the endpoints, the metadata
entry and the need for the key. `method: oidc` is still the seam for Keycloak:
when it arrives, `authorization_servers` names the realm instead of this
origin, and a client that follows the pointer changes nothing.

## Alternatives considered

| Alternative                                          | Why not                                                                                                                                                                                                                              |
|------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `method: oidc` with Keycloak now                     | The right answer where an identity provider already exists, and still planned. As the only answer it makes claude.ai wait for a realm, a registration policy that admits Claude, an audience mapper, and a second thing to run       |
| Authorization state in memory                        | Simpler to reason about, and wrong for two instances behind one route: a code issued by one is redeemed at the other. It would also sign everyone out at every deployment                                                            |
| Authorization state in a database or shared cache    | Buys per-token revocation and real rotation, at the price of the one-artifact, no-volume deployment on every platform this runs on                                                                                                   |
| Client ID Metadata Documents instead of registration | Claude prefers them when offered, and they need no registration endpoint - but the server has to fetch the client's document over the internet during sign-in, which an intranet-hosted server may not be able to do. Possible later |
| Signed JWTs as access tokens                         | Readable by any resource server with the public key, which is useful only when something other than this process checks them. Here nothing does, and HMAC needs no key pair                                                          |
| Static token in a claude.ai request header           | In beta for a limited set of organisations, and one credential for the whole organisation rather than one per person                                                                                                                 |
