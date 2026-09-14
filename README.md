# mkdocsgo

Serves a [MkDocs](https://www.mkdocs.org/) project: the built site over HTTP,
the documentation sources over the Model Context Protocol, or both from one
process and one port. One static Go binary, no authentication.

**MkDocs itself is not in the runtime.** Python builds the site when the image
is built; this binary serves what came out, plus a full-text index over the
Markdown sources. No Python, no plugins and no build step at run time.

> **Status: 0.1.0, an early release.** It runs, it is tested, and it serves a
> real documentation site. While the version is `0.x` the flag surface and the
> MCP payload shapes may still change; the changelog will say when they do.

## Documentation

| Topic                                              | File                                 |
|----------------------------------------------------|--------------------------------------|
| Serving the site: ETags, compression, cache policy | [SERVING.md](./SERVING.md)           |
| MCP: tools, search, anchors, origin validation     | [MCP.md](./MCP.md)                   |
| How it works inside                                | [ARCHITECTURE.md](./ARCHITECTURE.md) |
| Docker, Cloud Foundry, Kubernetes                  | [DEPLOYMENT.md](./DEPLOYMENT.md)     |
| What it replaces, and what it costs                | [COMPARISON.md](./COMPARISON.md)     |
| How a version is cut                               | [RELEASING.md](./RELEASING.md)       |
| What changed, and when                             | [CHANGELOG.md](./CHANGELOG.md)       |

A complete MkDocs repository wired to this server, with the Dockerfile, both
Cloud Foundry manifests and the Kubernetes manifests filled in:
**[mkdocsgo-example](https://github.com/kinjelom/mkdocsgo-example)**.

## Install

**A release binary.** No toolchain, and the checksum is published beside it:

```bash
VERSION=0.1.0
curl -fsSLO "https://github.com/kinjelom/mkdocsgo/releases/download/v${VERSION}/mkdocsgo_${VERSION}_linux_amd64.tar.gz"
curl -fsSLO "https://github.com/kinjelom/mkdocsgo/releases/download/v${VERSION}/mkdocsgo_${VERSION}_checksums.txt"
sha256sum --check --ignore-missing "mkdocsgo_${VERSION}_checksums.txt"
tar -xzf "mkdocsgo_${VERSION}_linux_amd64.tar.gz" mkdocsgo
```

Archives exist for linux, darwin and windows, amd64 and arm64.

**A container image:**

```bash
docker pull ghcr.io/kinjelom/mkdocsgo:0.1.0
```

**From source**, if you have Go 1.25 or newer:

```bash
go install github.com/kinjelom/mkdocsgo/cmd/mkdocsgo@latest
```

## Modes

```bash
mkdocsgo -mode site+mcp -project . -http 0.0.0.0:8080   # default
mkdocsgo -mode site     -project . -http 0.0.0.0:8080   # nginx replacement
mkdocsgo -mode mcp      -project . -http 0.0.0.0:8080   # MCP over HTTP
mkdocsgo -mode mcp      -project .                      # MCP over stdio
```

| Mode       | `/`            | `/mcp` | `/healthz` |
|------------|----------------|--------|------------|
| `site+mcp` | the built site | MCP    | yes        |
| `site`     | the built site | -      | yes        |
| `mcp`      | -              | MCP    | yes        |

`mcp` with no `-http` serves over stdio instead, which is what a local MCP
client launches. Every other mode needs `-http`.

| Flag             | Default          | Meaning                                                          |
|------------------|------------------|------------------------------------------------------------------|
| `-mode`          | `site+mcp`       | What to serve                                                    |
| `-project`       | `.`              | Directory holding `mkdocs.yml`                                   |
| `-site-dir`      | `<project>/site` | The built site                                                   |
| `-http`          | *(empty)*        | Address to listen on; falls back to `$DOC_PORT`, then `$PORT`    |
| `-search-limit`  | `8`              | Default search results (callers may override, capped at 50)      |
| `-allow-origin`  | -                | Additional allowed `Origin` for `/mcp`; repeatable               |
| `-no-resources`  | `false`          | MCP tools only, no per-page resources                            |
| `-no-access-log` | `false`          | Do not log HTTP requests                                         |
| `-healthcheck`   | -                | GET a URL, exit 0 on 2xx, then quit; `self` means own `/healthz` |
| `-mcp-probe`     | -                | Ask an MCP endpoint for its tool list, exit 0 if it answers      |
| `-version`       |                  | Print the version and exit                                       |

The last two exist because the runtime image is distroless: no shell, no
`curl`, no `wget`. The binary probes itself.

## Build and run

```bash
scripts/test.sh     # gofmt, go vet, go test
scripts/build.sh    # -> bin/mkdocsgo, bin/anchorcheck
scripts/dist.sh     # -> dist/, the cross-compiled release archives
scripts/image.sh    # the container image
scripts/release.sh  # all of it, plus tag, push and publish
```

Every script takes `--help`. `make` still works for the short development loop
(`make build`, `make test`, `make serve`); the scripts are what a release runs.

Go 1.25 or newer. If your `go` is older, `GOTOOLCHAIN=auto` (the default)
fetches the right toolchain by itself.

```bash
scripts/image.sh
docker run --rm -p 8080:8080 -v "$PWD:/project:ro" ghcr.io/kinjelom/mkdocsgo:0.1.0
```

## Releasing

```bash
scripts/release.sh minor            # 0.1.0 -> 0.2.0, tagged, built, published
scripts/release.sh patch --dry-run
```

The version lives in git tags and nowhere else; the changelog's `[Unreleased]`
section becomes the release notes; nothing leaves the machine until every local
step has succeeded. Details in [RELEASING.md](./RELEASING.md).

## Limits

- **No live reload.** Project and site are read once at startup; restart after
  a rebuild.
- **No writes.** Nothing here edits documentation.
- **It does not run MkDocs.** A `--8<--` include is served and indexed
  *literally*, and plugin-generated content does not exist for the index.
- **Anchors are a reimplementation** of Python-Markdown's, so they can drift.
  `anchorcheck` catches that.

The full account, including what you give up against nginx, is in
[COMPARISON.md](./COMPARISON.md).

## Layout

```
cmd/mkdocsgo/        the server
cmd/anchorcheck/     anchor verification against a built site
internal/web/        static site: manifest, ETags, gzip, headers, cache policy
internal/mkdocs/     mkdocs.yml, nav, Markdown sectioning, anchors
internal/index/      BM25 index, tokeniser, snippets
internal/mcpserver/  MCP tools and resources
scripts/             test, build, package, image, release
release.conf         where a release goes: GitHub, registry, platforms
examples/            a documentation project's own Dockerfile
testdata/            a small MkDocs project and a built site, used by the tests
```

## Licence

[MIT](./LICENSE). `scripts/dist.sh` ships the licence inside every release
archive, so a downloaded binary carries its terms with it.
