# Changelog

Every notable change, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

`scripts/release.sh` reads this file: it renames `[Unreleased]` to the version
being released, dates it, opens a fresh `[Unreleased]`, and uses the section it
just closed as the release notes and the annotated tag's message. So what you
write here while working is what the release says - add the entry with the
change, not at release time.

## [Unreleased]

## [0.3.0] - 2026-09-15

### Added

- **`**.` matches a domain at any depth**, next to `*.`, which still matches
  exactly one label. `**.in` covers `a.in` and `a.b.c.in` alike, and the longest
  suffix still wins, so `**.cfp1.i6e.in` beats `**.in`.

  It is for a policy stated in terms of a domain rather than a host - every
  internal foundation is internal, everything on the public one asks for
  credentials - where the alternative is listing foundations as they are
  created and having the newest one answer 403 until somebody notices. Spelled
  with two stars because the wide match is the one worth writing on purpose:
  nothing that already used `*.` changes meaning.

## [0.2.0] - 2026-09-15

### Added

- **Zones**: `mkdocsgo.yml` beside `mkdocs.yml` maps the addresses the server
  answers on to a named zone, and a zone to `public`, `restricted` or `off`.
  Several addresses may share one; the most specific pattern wins; an address
  no zone claims is refused. This is what lets one deployment serve an intranet
  route openly and an internet route only to named principals. A project
  without the file behaves exactly as it did before - everything public, no
  challenge - so the upgrade is a no-op until you write one. `-config` points
  at the file elsewhere.
- A restricted zone asks a browser for HTTP Basic credentials and an agent for
  `Authorization: Bearer`. The MCP `401` carries the `WWW-Authenticate`
  challenge the specification asks for, pointing at RFC 9728 protected resource
  metadata the server publishes at `/.well-known/oauth-protected-resource/mcp`
  - so the day a zone moves to Keycloak, a client that already follows the
  pointer needs no change. `method:` is that seam, and `oidc` is refused at
  startup rather than ignored.
- Credentials are stored as hashes - Argon2id for a password, SHA-256 for a
  token - which is why the file is not a secret and needs no encryption. Bcrypt
  hashes from `htpasswd -B` are accepted as well. `-new-token` and
  `-hash-password` mint them and print the line to paste.
- The access log names the zone and, where there is one, the principal and the
  credential id it came in on. The `Authorization` header is never logged.
- `AUTH.md`, and `docs/adr/0001-zone-based-authentication.md` for why it is
  shaped this way - including what was rejected: zones that also scope content,
  and an encrypted credentials file.

### Changed

- In a restricted zone every `Cache-Control` the site would have sent as
  `public` is sent as `private`, and responses carry
  `X-Robots-Tag: noindex, nofollow`. The freshness is unchanged; a shared cache
  loses the right to store the page and hand it to the next person.
- Configuration errors stop the server. A restricted zone with no principals, a
  principal nobody lets in, a plaintext password where a hash belongs, one host
  in two zones, an unknown key - each fails at startup rather than at some
  later request.

## [0.1.1] - 2026-09-14

### Fixed

- `get_page` and `get_section` now carry the Markdown in the structured result
  as well as in the content block. Both tools declare an output schema, so a
  client may render `structuredContent` and never read the content block - and
  such a client saw the metadata and none of the text. The two channels now
  hold the same Markdown. The `markdown` field is additive, so a client already
  reading the structured result keeps every field it had. `search_docs` and
  `list_pages` were never affected: their structured results already carried
  the text, which is why only these two tools looked empty.

## [0.1.0] - 2026-09-14

The first release. Everything below is new.

While the version is `0.x` the flag surface and the MCP payload shapes may
still change. A change that breaks either will bump the minor version and say
so here.

### Added

- Serving a built MkDocs site over HTTP: content-hash ETags, compression done
  once at startup, security headers, a per-path cache policy, directory URLs,
  the project's own `404.html`, range requests.
- An MCP server over Streamable HTTP and stdio, with four section-shaped tools
  (`search_docs`, `get_section`, `get_page`, `list_pages`) and one resource per
  page.
- A BM25 index over sections, weighting heading terms and preserving version
  strings through tokenisation.
- `site`, `mcp` and `site+mcp` modes from one binary.
- `-healthcheck` and `-mcp-probe`, so a distroless image with no shell can
  still be probed.
- `cmd/anchorcheck`, which compares computed anchors against a real built site.
- `scripts/release.sh` and its parts (`test.sh`, `build.sh`, `dist.sh`,
  `image.sh`): cross-compiled archives with checksums, a container image, an
  annotated tag and a GitHub release from one command.
- Documentation: `SERVING.md` (the site half), `MCP.md` (the agent half),
  `ARCHITECTURE.md` (how it works inside), `DEPLOYMENT.md` (Docker, Cloud
  Foundry with both a Docker application and the binary buildpack, and
  Kubernetes), `COMPARISON.md` (what this replaces and what it costs) and
  `RELEASING.md`.
- `mkdocsgo-example`, a companion repository: a complete MkDocs project wired
  to this server, with every deployment manifest filled in.
- The MIT licence, shipped inside every release archive.
