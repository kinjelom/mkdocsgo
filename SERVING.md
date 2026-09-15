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
