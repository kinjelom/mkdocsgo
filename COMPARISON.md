# What this replaces, and what it costs

mkdocsgo is not a different way to *build* documentation. MkDocs still builds
it, with the same `mkdocs.yml`, the same theme and the same plugins. What
changes is what happens afterwards - and whether anything but a browser can
read the result.

This page is the honest version: what you gain, what you pay, and when you
should not use it at all.

## The setups it is measured against

Four things people actually do with a MkDocs project.

|                                  | What runs in production                   | Gets you                                                                      |
|----------------------------------|-------------------------------------------|-------------------------------------------------------------------------------|
| **A. `mkdocs serve`**            | The Python dev server                     | Live reload. Single-threaded, no cache headers, explicitly not for production |
| **B. Static host**               | GitHub Pages, S3 + CDN, Netlify           | Cheap, fast, nothing to operate                                               |
| **C. `mkdocs build` + nginx**    | An nginx container holding `site/`        | The usual enterprise answer. Full control, an `nginx.conf` to maintain        |
| **D. `mkdocs build` + mkdocsgo** | One Go binary holding `site/` and `docs/` | C, plus an MCP server, minus the configuration                                |

B is the right answer for a public open-source site and this changes nothing
about that. The comparison that matters is **C against D**, because they solve
the same problem: serving a built site from your own infrastructure.

## Side by side

|                                | C - nginx container                                      | D - mkdocsgo                                                        |
|--------------------------------|----------------------------------------------------------|---------------------------------------------------------------------|
| Processes in the image         | nginx master + worker                                    | one                                                                 |
| Image size (real project)      | ~77 MB                                                   | ~22 MB                                                              |
| What is in the image           | alpine, nginx, its modules, the site                     | distroless, one static binary, the site, the Markdown               |
| Shell in the runtime image     | yes                                                      | no                                                                  |
| Memory at rest                 | ~15 MB                                                   | binary + the gzipped site held in memory (1.6 MB for a 6.9 MB site) |
| Configuration to maintain      | `nginx.conf`, a cache `map`, a headers list, per project | flags                                                               |
| ETag across replicas           | derived from inode + mtime - **replicas disagree**       | content hash - replicas agree                                       |
| Compression                    | per request, or a build step that writes `.gz` files     | once, at startup, in memory                                         |
| Readable by an agent           | no                                                       | `/mcp`, same port                                                   |
| Liveness endpoint              | whatever you configure                                   | `/healthz`, always                                                  |
| Live reload                    | no                                                       | no                                                                  |
| TLS, rewrites, redirects       | yes, it is nginx                                         | no - put it behind something                                        |
| Authentication                 | `auth_basic`, plus whatever module you add                | per-zone Basic and bearer tokens, from the documentation repository |

### The ETag difference, concretely

nginx builds an ETag from a file's inode and modification time. Two containers
running the same build have different inodes, so they emit different ETags for
identical bytes. A reader whose request lands on the other replica re-downloads
everything the first one had already cached. It is invisible in a single-replica
deployment and annoying in a load-balanced one.

mkdocsgo hashes the content at startup. Same bytes, same ETag, every replica.

## What you gain

**One artifact, one version, one deployment.** The site and the thing that
serves it are the same image, built together and versioned together. There is
no "which nginx config is that environment running" question, because there is
no nginx config.

**A much smaller attack surface.** The runtime image is distroless: no shell,
no package manager, no interpreter, no nginx modules. Almost every CVE that
would otherwise land in your weekly scan report comes from packages that are
not there. The trade is that you cannot `exec` into a running container to look
around - which is the same property, seen from the other side.

**Documentation an agent can read.** This is the part the nginx setup cannot do
at any configuration setting. `/mcp` exposes the sources as four
section-shaped tools, so an assistant answering "what does the `M*` flag mean
on invoice lines" retrieves the one relevant section rather than a 4000-line
page - or rather than scraping HTML and guessing. Same process, same port, same
deployment.

**Configuration that cannot drift.** The cache policy and the security headers
are in the binary, identical in every project that uses it. In setup C they are
in a file that gets copied between repositories and edited in one of them.

**It costs less to run.** 22 MB instead of 77, one process instead of two,
64-128 MB of memory instead of a container sized for nginx plus headroom. Not
a large sum - but a cheaper thing to run in twenty places.

## What you pay

**You still need Python to author.** mkdocsgo does not build anything and has
no live reload. `mkdocs serve` remains the authoring loop, so the Python
toolchain does not leave the developer's machine or the build - only the
runtime.

**It does not run MkDocs, so the MCP index is a little dumber than the site.**
The server reads Markdown sources directly. A `--8<-- "examples/report.json"`
snippet include is indexed **literally**, as that line of text; the file it
points at is never searched. Macros do not evaluate and plugin-generated pages
do not exist for the index. The published site is unaffected - MkDocs already
expanded all of it at build time - but an agent searching for a value that only
exists inside an included file will not find it.

**Anchors are a reimplementation and can drift.** Heading anchors are computed
the way Python-Markdown's `toc` extension computes them. It matches today, and
`cmd/anchorcheck` compares the two against a real built site so a drift is
caught in CI rather than in a broken deep link. It is still a reimplementation.

**You lose nginx's general-purpose configurability.** No TLS termination, no
rewrites, no redirect maps, no rate limiting, no `try_files`. All of
it lives in front of the server instead - the CF router, an Ingress, a reverse
proxy - which is where most deployments already have it. If you were using
nginx for more than serving files, you still need nginx.

**A restart is the only way to pick up a change.** Project and site are read
once at startup. That is the same lifecycle as the image carrying them, so in a
container deployment it costs nothing; running the binary directly on a VM, it
means a restart after every rebuild.

**`exclude_docs` is honoured for the common cases only.** One pattern per line,
directory prefixes and shell globs. Negation (`!`) is ignored rather than
half-implemented, because getting it wrong would index more than you asked for,
not less.

**Two search implementations now exist in one deployment.** The site keeps
Material's client-side search for readers; the MCP half has its own BM25 index
for agents. They are built from different inputs and will not always rank the
same way. They serve different clients, so this rarely matters - but "the
search box found it and the agent did not" has an explanation, and this is it.

**It is one more thing you own.** nginx is understood by every operations team
on earth. This is a Go binary maintained in one repository. That is why the
release process is scripted, versioned and checksummed rather than left to
`go build` on someone's laptop - but it is still a dependency with your name
on it.

## When not to use it

- **A public site on GitHub Pages or a CDN.** Setup B is cheaper, faster and
  has nothing to operate. There is no server to add MCP to.
- **You need nginx for something else** - TLS, rewrites, proxying other
  backends. Then you have nginx anyway; serving `site/` from it too is free.
- **Nothing will ever read the documentation but a browser.** Then D's headline
  feature is unused, and the argument narrows to image size and ETags - real,
  but not usually enough on its own.
- **The build is plugin-heavy in ways the sources do not show.** If most of the
  useful content is generated by macros or plugins at build time, the MCP index
  sees the templates rather than the result, and the agent-facing half is worth
  much less.

## When it is clearly right

- Internal or customer-facing documentation you host yourself, **and** people
  are pointing AI assistants at it. That is the case it was built for.
- A fleet of documentation sites where an `nginx.conf` has been copied between
  repositories and has quietly diverged.
- Anywhere the image size, the process count or the CVE surface of a
  documentation container is scrutinised more than it deserves to be.

## The honest summary

Against a static host you are adding a server, which needs a reason. The reason
is `/mcp`.

Against an nginx container you are removing a technology, a configuration file
and 55 MB, gaining stable ETags and an MCP endpoint, and giving up nginx's
other capabilities - which most documentation deployments were not using.
