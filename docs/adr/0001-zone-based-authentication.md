# 1. Authentication by zone, in the process, with hashes in the repository

- **Status:** accepted
- **Date:** 2026-09-15
- **Applies to:** mkdocsgo 0.2.0

## Context

Until 0.1.1 the server had no authentication at all, and said so: the
documentation it serves is published documentation, and anything else belongs
behind whatever the platform provides.

That stopped being true for the case that prompted this: **one documentation
set reachable at two kinds of address** - an intranet route where everyone
inside may read it, and an internet route where only named partners may. The
pages are the same pages. What differs is who can reach the address.

Three properties made the platform answer unsatisfying:

1. **Cloud Foundry has no per-route authentication.** An Ingress annotation or
   a ForwardAuth middleware solves this in Kubernetes; on CF the equivalent is
   a route service, which is an application to build, deploy and keep alive.
2. **The mapping from address to policy belongs to the documentation
   repository.** It is reviewed with the content, travels with the image, and
   is identical on a laptop and in production - which is the same argument that
   put the cache policy and the security headers in this binary rather than in
   a per-project nginx fragment.
3. **A browser-centric proxy does not serve `/mcp` well.** oauth2-proxy answers
   an unauthenticated request with a redirect to a login page. An agent needs a
   `401` with a `WWW-Authenticate` header it can act on.

## Decision

**Zones in the process.** `mkdocsgo.yml`, beside `mkdocs.yml`, maps addresses
to a named zone, and a zone to `public`, `restricted` or `off`. Its absence
keeps the old behaviour exactly. The format is in [AUTH.md](../../AUTH.md).

**A zone is access, not visibility.** Content that must differ is a separate
build and a separate deployment. Filtering pages per zone at request time was
considered and rejected: Material's `search_index.json` carries the full text
of every page, so anything short of rebuilding the index would leak what it
claimed to hide.

**Fail closed.** An address no zone claims is refused, a misconfiguration stops
the server from starting, and an unimplemented `method` is an error rather than
something quietly ignored.

**Hashes in the repository, and no encryption layer.** An Argon2id PHC string
for a password, a SHA-256 digest for a token. The file is therefore not a
secret, and there is no key to distribute, rotate or lose, and no decryption
step in the pipeline. This replaced an earlier plan for an encrypted
`*.enc.yaml` holding the credentials themselves - which would have made
encryption the only thing between a leaked file and a working password, and
added a key-management problem to a documentation server.

**`Authorization` on both surfaces.** Basic for the browser, Bearer for MCP,
with the `401` carrying `WWW-Authenticate` and, on `/mcp`, a pointer to RFC 9728
protected resource metadata this server publishes. No custom `X-API-Key`
header: it is in no specification and no client's discovery path.

**`method` is a seam, and today has one value.** `static` verifies against the
hashes in the file. `oidc` and `forwarded` are named in the schema, refused at
startup, and are how Keycloak arrives without the file changing shape.

## Consequences

**Gained.** One artifact still. The policy is reviewed in the same pull request
as the pages it protects, and is the same on every platform. An agent gets a
challenge it can act on, and the challenge already points at the metadata
document that will name Keycloak.

**Paid.**

- This binary now verifies credentials, which is a category of code it did not
  contain before: two new dependencies (`x/crypto`, `x/term`) and the
  obligations that come with a login path - constant-time comparison, a delay
  on rejection, and a cap on how much memory an anonymous caller can make the
  process spend.
- Argon2id costs 19 MiB per verification, capped at two at a time. A container
  sized at 128 MiB for a public site wants more headroom once a zone is
  restricted.
- HTTP Basic has no logout. It is the cheapest thing a browser and a static
  configuration can agree on, and it is the half expected to move to Keycloak
  first.
- One policy per zone covers both surfaces. A zone public for browsers and
  restricted for agents is not expressible today.

**Reversible.** Deleting `mkdocsgo.yml` restores the previous behaviour
exactly, and putting a gateway in front remains possible: the 2026-07-28
`Mcp-Method` and `Mcp-Name` headers still let one authorise `/mcp` without
parsing bodies.

## Alternatives considered

| Alternative                                           | Why not                                                                                                                                                                                                                                                   |
|-------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| oauth2-proxy or Authelia in front                     | Solves the browser well and Keycloak immediately, but redirects instead of challenging on `/mcp`, and moves the policy out of the repository that owns the addresses. Kept as the intended route for the browser half later, through a `forwarded` method |
| Ingress / ForwardAuth / Istio policy                  | Declarative and per-host, but unavailable on Cloud Foundry and different in every environment                                                                                                                                                             |
| Zones that also scope content                         | Rejected: `search_index.json` and the sitemap would have to be rebuilt per zone. Separate builds do this correctly and cost nothing but a pipeline                                                                                                        |
| Encrypted credentials file (`*.enc.yaml`, SOPS + age) | Rejected in favour of storing hashes: encryption would have been the only barrier, and it adds a key to manage in every environment. Hashing removes the secret instead of hiding it                                                                      |
| An `X-API-Key` header for agents                      | Rejected: not in the MCP specification, not in any client's discovery path, and a second way in is a second way to get the policy wrong                                                                                                                   |
