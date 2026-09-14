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
