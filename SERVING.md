# Serving the site

The `/` half: a replacement for the nginx container that would otherwise front
the same files, with the same policy and two improvements.

Active in `-mode site` and `-mode site+mcp`.

## What it does

- **ETags are content hashes**, computed at startup. nginx derives them from
  inode and mtime, so two replicas of the same build disagree and a client that
  reaches a different one downloads again. Here they match everywhere.
- **Compression happens once**, at startup, not per request. Only compressible
  types above 256 bytes are stored gzipped, and only if gzip actually made them
  smaller.
- Security headers on every response, success or error, declared in one list.
- Directory URLs (`/page/` -> `/page/index.html`) and the project's own
  `404.html`, with `no-cache`.
- Range requests and `If-Modified-Since` for uncompressed responses.
- `/healthz` answers 200 in every mode.

## Cache policy

The same one the replaced nginx `map` spelled out, except that it lives in the
binary and is therefore identical in every project that uses it.

| Path                                           | `Cache-Control`               | Why                                                   |
|------------------------------------------------|-------------------------------|-------------------------------------------------------|
| `/assets/stylesheets/`, `/assets/javascripts/` | `max-age=31536000, immutable` | Material fingerprints these filenames                 |
| `/assets/vendor/`                              | `max-age=86400`               | Unversioned path, so a bump must still reach browsers |
| images, fonts                                  | `max-age=604800`              | Not fingerprinted                                     |
| everything else                                | `no-cache`                    | Revalidated cheaply against a content-hash ETag       |

`no-cache` does not mean "do not cache". It means "revalidate before use", and
revalidating against a content-hash ETag costs one 304.

**In a restricted zone every line above becomes `private`**, and each response
carries `X-Robots-Tag: noindex, nofollow`. The freshness is unchanged - a
fingerprinted asset is still immutable to the browser that fetched it - but a
shared cache loses the right to store it and hand it to the next person.
[AUTH.md](./AUTH.md) has the rest.

## Markdown sources

A zone with `markdown: true` in [`mkdocsgo.yml`](./AUTH.md#the-file) offers
every page's Markdown source next to its HTML - for a person who wants the
text, and for an agent that fetches URLs rather than speaking MCP.

```yaml
zones:
  intranet:
    hosts: [docs.intranet.example.com]
    access: public
    markdown: true
```

**At the page's address with `.md` added.** Every spelling of that leads to
the same file:

| Page                  | Its Markdown                                                                                          |
|-----------------------|-------------------------------------------------------------------------------------------------------|
| `/guides/deploy/`     | `/guides/deploy.md` - the path under `docs_dir` - and `/guides/deploy/.md`, `/guides/deploy/index.md` |
| `/guides/`            | `/guides/index.md`, `/guides.md`, `/guides/.md`                                                       |
| `/`                   | `/index.md`                                                                                           |
| `/guides/deploy.html` | `/guides/deploy.md`, `/guides/deploy.html.md` - a site built without directory URLs                   |

**One section, with `?section=`.** Any of those addresses followed by
`?section=<anchor>` returns only that heading and the text beneath it, up to
the next heading - what the MCP tool `get_section` returns, from the same code:

```
/guides/deploy.md?section=rolling-updates     the "Rolling updates" section
/guides/deploy.md?section=%23rolling-updates  the same; a copied #anchor works
/guides/deploy.md?section=                    the text before the first heading
```

The anchors are the ones in the page's URLs and table of contents. An anchor
the page does not have is a `404` whose text lists the ones it does, so an
agent can correct itself. A fragment - `/guides/deploy.md#rolling-updates` -
cannot do this: a browser never sends it to the server.

**On the page itself.** On a site built with Material, every page gets a
download button among Material's own page actions, at the top right of the
content, where "edit this page" would be. Every page, Material or not, gets
`<link rel="alternate" type="text/markdown" href="…">` in its head, which is
how an agent reading the HTML finds the source without guessing.

**Browsers see it, agents get it as Markdown.** A request that accepts
`text/html` - a browser following a link or an address typed by hand - gets
the source as `text/plain`, which every browser shows rather than saves. Any
other request gets `text/markdown`. Same bytes, two ETags, `Vary: Accept`.

What that costs and guarantees:

- **Only built pages.** A source whose page MkDocs did not build - excluded by
  `exclude_docs`, or a draft - has no Markdown here either. The source is
  published exactly where its HTML is. The MCP half applies the same rule, so
  both halves publish the same set of pages - see [MCP.md](./MCP.md#which-pages).
- **The source as written**: front matter, HTML comments, snippet include
  lines and all. That is the reason it is a per-zone choice and off by default.
  It is also what the MCP tools already return, so a zone that serves `/mcp`
  has handed it out all along.
- **Exactly as protected as the page.** The decision is made below the zone
  check, so a restricted zone asks for the same credentials first.
- **Every other zone sees what MkDocs built**, byte for byte - no button, no
  link, and `.md` addresses answer 404. The pages with the button are a second
  copy of each HTML page made at startup, with their own ETags and compressed
  copies, held only when some zone offers Markdown.
- **`docs/` must be deployed**, even with `-mode site`: that is where the
  sources come from.

## Cost at rest

For a 77-file, 6.9 MB site: 1.6 MB held gzipped in memory, and a startup well
under a second including hashing and compressing everything.

Startup is when all of it happens. Afterwards the manifest is immutable, which
is why both halves can be read concurrently without locking - see
[ARCHITECTURE.md](./ARCHITECTURE.md).

## Path traversal

Not possible. Lookups resolve against the manifest built at startup, never
against a caller-supplied filesystem path. The same is true of the MCP half,
which resolves against the page set loaded from `docs_dir`.

## What it will not do

No TLS termination, no rewrites, no redirect maps, no rate limiting, no
`try_files`. All of that belongs in front of the server - the Cloud Foundry
router, an Ingress, a reverse proxy - which is where most deployments already
have it.

Authentication is the one thing that moved in: a zone can ask for HTTP Basic
credentials, because which address gets which policy is a property of the
documentation repository and not of the platform it happens to run on. It is
whole-zone and nothing more - no per-path rules, no roles, no sessions.

If you were using nginx for more than serving files, you still need nginx. See
[COMPARISON.md](./COMPARISON.md).
