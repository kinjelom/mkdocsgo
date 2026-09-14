# ---------------------------------------------------------------------------
# mkdocsgo - the server binary, without any documentation
#
# Two stages: build a static binary, then copy it into a distroless image that
# holds nothing else - no shell, no package manager, no interpreter.
#
# This image carries no content. Either mount a project at /project:
#
#     docker run --rm -p 8080:8080 -v "$PWD:/project:ro" mkdocsgo
#
# where /project holds mkdocs.yml, docs/ and a built site/ - or bake a project
# in, which is what a documentation repository should do. See
# examples/project.Dockerfile for that, including the MkDocs build step.
# ---------------------------------------------------------------------------

ARG GO_IMAGE=golang:1.25-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS builder

ARG VERSION=dev
ENV CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/mkdocsgo ./cmd/mkdocsgo

FROM ${RUNTIME_IMAGE} AS runtime

ARG VERSION=dev

LABEL org.opencontainers.image.title="mkdocsgo" \
      org.opencontainers.image.description="Serves a MkDocs site and an MCP server for it from one process" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=builder /out/mkdocsgo /mkdocsgo

EXPOSE 8080

ENTRYPOINT ["/mkdocsgo"]
CMD ["-mode", "site+mcp", "-project", "/project", "-http", "0.0.0.0:8080"]
