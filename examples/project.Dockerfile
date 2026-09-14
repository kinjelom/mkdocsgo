# ---------------------------------------------------------------------------
# Template: a documentation repository's own image.
#
# Copy this into a MkDocs project, adjust the ARGs, and it produces one
# image that serves the site and the MCP server from a single process.
#
# Three stages, and the point of the split is what does NOT reach the last one:
#
#   1. python   runs `mkdocs build`. The whole documentation toolchain lives
#               and dies here.
#   2. golang   compiles the server.
#   3. runtime  distroless: the binary, the built site, and the Markdown
#               sources the MCP index is built from. No Python, no Node, no
#               shell, no package manager.
#
# The Markdown sources are copied in because the MCP server indexes the
# author's original text, not rendered HTML. They are a fraction of the size of
# the site.
# ---------------------------------------------------------------------------

ARG PYTHON_IMAGE=python:3.13-slim
# Pin it. `:latest` in a build stage means this image changes when someone
# else cuts a release, and a rollback then has nothing to go back to.
ARG MKDOCSGO_IMAGE=ghcr.io/kinjelom/mkdocsgo:0.1.0
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

# --- 1. Build the site ------------------------------------------------------
FROM ${PYTHON_IMAGE} AS site

ENV PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /build
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY mkdocs.yml ./
COPY docs/ ./docs/

# --strict turns every MkDocs warning - a broken internal link, a page missing
# from the navigation - into a build failure, so a broken site cannot ship.
RUN mkdocs build --strict --site-dir /site

# --- 2. Take the server binary ----------------------------------------------
FROM ${MKDOCSGO_IMAGE} AS server

# --- 3. Assemble ------------------------------------------------------------
FROM ${RUNTIME_IMAGE} AS runtime

ARG VERSION=dev

LABEL org.opencontainers.image.version="${VERSION}"

COPY --from=server /mkdocsgo /mkdocsgo
COPY --from=site /site /project/site
COPY mkdocs.yml /project/mkdocs.yml
COPY docs/ /project/docs/

EXPOSE 8080

ENTRYPOINT ["/mkdocsgo"]
CMD ["-mode", "site+mcp", "-project", "/project", "-http", "0.0.0.0:8080"]
