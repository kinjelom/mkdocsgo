# MCP

The `/mcp` half: the documentation sources, served to agents over the Model
Context Protocol.

Active in `-mode mcp` and `-mode site+mcp`. With `-mode mcp` and no `-http`, it
speaks over stdio instead, which is what a local MCP client launches.

## Why sections rather than pages

A specification page runs to thousands of lines. Handing all of it back for a
three-word question spends an agent's context on text it did not ask for.

Everything is section-shaped: `search_docs` ranks headings and the prose
beneath them, results carry the navigation breadcrumb and the heading anchor,
and `get_section` returns that one section.

## Tools

| Tool          | Returns                                                                     |
|---------------|-----------------------------------------------------------------------------|
| `search_docs` | Best matching **sections**, each with breadcrumb, anchor, snippet and score |
| `get_section` | One section as Markdown - the cheap way to read                             |
| `get_page`    | A whole page as its original Markdown, plus its heading list                |
| `list_pages`  | Every page with breadcrumb and heading count; `prefix` filters              |

All four are annotated read-only and idempotent. A bad path or anchor comes
back as a *tool* error with a hint, not a protocol error, so the model can
correct itself instead of failing the turn.

Each page is also an MCP **resource** at `docs://<path>` serving the source
Markdown. Tools come first on purpose: every client implements tools, resource
support is uneven, and a resources-only server is invisible to a client that
speaks only tools. `-no-resources` turns the resources off and keeps the tools.

## Protocol

Streamable HTTP at `/mcp`, **stateless**: no `Mcp-Session-Id`, so any instance
answers any request and no load balancer has to pin a session. This is the
sessionless direction of the 2026-07-28 spec.

Transport and negotiation come from the official
[`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk),
which supports 2026-07-28 with backward compatibility down to 2024-11-05.

## Search

A hand-rolled BM25 index over sections - no external search service, no cgo,
which is what keeps the binary static and the footprint flat.

- Heading terms weigh more than body terms; a section containing the query
  verbatim gets a bonus, which is what makes identifier searches like
  `InvoiceNumber` land correctly.
- Version strings survive tokenisation: `1.0.0` stays one token instead of
  three single digits the index would discard.
- Snippets are a window around the first match, trimmed to word boundaries.

`-search-limit` sets the default number of results. Callers may override it,
capped at 50.

## Registering it with a local client

```json
{
  "mcpServers": {
    "docs": {
      "command": "mkdocsgo",
      "args": ["-mode", "mcp", "-project", "/path/to/mkdocs-project"]
    }
  }
}
```

stdout carries JSON-RPC in stdio mode, so every diagnostic goes to stderr. One
stray byte on stdout breaks the client handshake.

The site does not need to be built for this. The MCP half reads the Markdown in
`docs_dir`, not the rendered HTML.

## Origin validation

`Origin` is validated on `/mcp`. That is DNS-rebinding defence, **not access
control**: without it, a page the user happens to visit could drive a server
bound to their loopback interface.

- No `Origin` header - every non-browser MCP client - passes.
- Loopback origins pass.
- Anything else gets 403 unless named with `-allow-origin`, which is
  repeatable.

It runs outside the zone check and before it, so a rebinding attempt is
rejected before the process spends anything on verifying a credential.

## Authentication

Nothing here needs credentials unless the address it arrived at belongs to a
restricted zone - see [AUTH.md](./AUTH.md) for the whole of it. What the MCP
half contributes:

- the token travels in `Authorization: Bearer`, which is what the
  specification asks for and what clients already send;
- an unauthenticated request gets `401` with
  `WWW-Authenticate: Bearer realm="…", resource_metadata="…"`;
- that pointer resolves to RFC 9728 protected resource metadata this server
  publishes at `/.well-known/oauth-protected-resource/mcp`, outside the zone,
  because a client reads it in order to learn how to authenticate.

```json
{ "mcpServers": { "docs": {
  "type": "http",
  "url": "https://docs.example.com/mcp",
  "headers": { "Authorization": "Bearer mkd_…" }
}}}
```

**stdio has no zones**: no `Host` to match, and the client is a process the
user started.

## Anchors

Heading anchors are reproduced the way Python-Markdown's `toc` extension
generates them, because that is what MkDocs links and what a reader's URL
fragment says: NFKD-normalise, drop non-ASCII, strip everything that is not a
word character, whitespace or a hyphen, lowercase, collapse runs into single
hyphens, deduplicate repeats with `_1`, `_2`.

Intra-word underscores are kept - `IMAGE_VERSION` anchors as `image_version`,
not `imageversion`.

Since that is a reimplementation it can drift. `anchorcheck` compares it
against a real built site:

```bash
mkdocs build
anchorcheck -project . -site site
# 16 pages, 78 anchors compared, 0 mismatched
```

Worth wiring into CI next to the docs build.

## What the index does not see

It never runs MkDocs. No plugins load, no macros evaluate, no HTML renders.

A `--8<-- "examples/report.json"` snippet include is therefore indexed
**literally**, as that line of text; the file it points at is never searched.
The published site is unaffected - MkDocs already expanded it at build time -
but a value that exists only inside an included file will not be found.

`exclude_docs` is honoured for the common cases: one pattern per line,
directory prefixes and shell globs. Negation (`!`) is ignored rather than
half-implemented, because getting it wrong would index more than you asked for.
