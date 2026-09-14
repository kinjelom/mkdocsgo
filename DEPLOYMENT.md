# Deployment

Three ways to run mkdocsgo - Docker, Cloud Foundry, Kubernetes - and one thing
they all deploy: a single static binary next to a built MkDocs project.

Ready-to-use manifests live in the example repository,
[mkdocsgo-example](https://github.com/kinjelom/mkdocsgo-example), under
`deploy/`. What follows explains what they say and why.

## What you are deploying

The runtime needs a directory laid out the way MkDocs already lays one out:

```
/project
|-- mkdocs.yml      site_name, docs_dir, exclude_docs, nav
|-- docs/           the Markdown sources - what the MCP index reads
\-- site/           the built site - what browsers get
```

...plus the `mkdocsgo` binary. **Python is not part of the runtime.** MkDocs
runs once, wherever you build, and the result travels as files.

`docs/` is there because the MCP half indexes the author's Markdown, not
rendered HTML. It is a fraction of the size of `site/`. If you deploy
`-mode site` only, you can leave it out.

### The port

`-http` wins; without it the server reads `$DOC_PORT`, then `$PORT`, and binds
`0.0.0.0` on whichever it finds.

That order is what makes one image run unchanged in all three platforms.
Cloud Foundry assigns `$PORT` and expects the app to obey it; Kubernetes and
Docker let you fix the port yourself. Nothing has to expand a variable in a
shell - which matters, because the runtime image has no shell to expand it.

### Health

`/healthz` answers 200 in every mode, including `-mode mcp`. Use it as the
liveness and readiness probe everywhere; a bare TCP check would report a
container healthy whose site failed to load.

---

## 1. Docker

### The server image

`ghcr.io/kinjelom/mkdocsgo:<version>` carries the binary and nothing else: no
shell, no package manager, no documentation. Two ways to use it.

**Mount a project** - good for a local look at a site you have already built:

```bash
mkdocs build                                   # produces ./site
docker run --rm -p 8080:8080 \
  -v "$PWD:/project:ro" \
  ghcr.io/kinjelom/mkdocsgo:latest
```

**Use it as a build stage** - what a documentation repository should do. The
project bakes itself in and ships one self-contained image:

```dockerfile
ARG PYTHON_IMAGE=python:3.13-slim
ARG MKDOCSGO_IMAGE=ghcr.io/kinjelom/mkdocsgo:0.1.1
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${PYTHON_IMAGE} AS site
WORKDIR /build
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY mkdocs.yml ./
COPY docs/ ./docs/
RUN mkdocs build --strict --site-dir /site

FROM ${MKDOCSGO_IMAGE} AS server

FROM ${RUNTIME_IMAGE} AS runtime
COPY --from=server /mkdocsgo /mkdocsgo
COPY --from=site   /site     /project/site
COPY mkdocs.yml /project/mkdocs.yml
COPY docs/      /project/docs/
EXPOSE 8080
ENTRYPOINT ["/mkdocsgo"]
CMD ["-mode", "site+mcp", "-project", "/project", "-http", "0.0.0.0:8080"]
```

The full, commented version is [examples/project.Dockerfile](./examples/project.Dockerfile).
Pin `MKDOCSGO_IMAGE` to a version - `:latest` in a build stage means your image
changes when someone else releases.

Python exists only in stage 1. What reaches the result is the binary, the built
site and the Markdown sources: about 22 MB in total for a real project.

### Health check inside the image

The image is distroless, so `curl` and `wget` are not there to call. The binary
probes itself:

```dockerfile
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/mkdocsgo", "-healthcheck", "self"]
```

`self` resolves the port from `$DOC_PORT`/`$PORT` inside the binary, because
`HEALTHCHECK` in exec form has no shell to expand a variable. To check the MCP
half as well - it answers POST, so a GET tells you nothing:

```bash
docker exec <container> /mkdocsgo -mcp-probe http://127.0.0.1:8080/mcp
```

---

## 2. Cloud Foundry

Two ways, and which one is right depends on one question: **do you have a
container registry the foundation can pull from?**

|                  | Docker application           | Binary buildpack                           |
|------------------|------------------------------|--------------------------------------------|
| Needs a registry | yes                          | no                                         |
| What you push    | an image reference           | a directory of files                       |
| Who builds       | your CI                      | your CI (the site) + CF (the droplet)      |
| Runs as          | your image, distroless       | `cflinuxfs4` stack + your binary           |
| Image scanning   | whatever scans your registry | CF's stack patching                        |
| Rollback         | re-push an older tag         | `cf rollback` to an earlier droplet        |
| Best when        | you already publish images   | you do not, or the registry is unreachable |

Both give the same process listening on `$PORT`.

### Option A - Docker application

```yaml
applications:
  - name: mkdocsgo-example

    docker:
      # Built and published beforehand. Cloud Foundry pulls it; pushing
      # builds nothing.
      image: ((docker_image))

    instances: 2
    memory: 128M
    disk_quota: 512M

    health-check-type: http
    health-check-http-endpoint: /healthz
    health-check-invocation-timeout: 5

    routes:
      - route: docs.apps.example.com

    env:
      # The server reads DOC_PORT first, then PORT. Setting it pins the
      # listener to the port the image exposes, whatever the platform does.
      DOC_PORT: 8080
```

`((docker_image))` is a manifest variable on purpose: the version has exactly
one home, and `cf push -f manifest.yml` run by hand fails on the missing
variable rather than deploying a stale tag - or, with no image at all,
uploading the repository as application source.

```bash
cf push -f deploy/cf/manifest-docker.yml --var docker_image=ghcr.io/kinjelom/mkdocsgo-example:0.1.0
```

A private registry needs `CF_DOCKER_PASSWORD` in the environment and
`--docker-username`. Pass the password through the environment, never as an
argument - an argument is visible in the process list.

### Option B - binary buildpack

The binary buildpack does no compilation: it takes the directory you push and
runs the command you name. So the directory must already contain everything -
the Linux binary, `mkdocs.yml`, `docs/` and the built `site/`.

Assemble it in CI, then push that directory rather than the repository:

```
dist/cf/
|-- mkdocsgo        # linux/amd64, from the GitHub release
|-- mkdocs.yml
|-- docs/
\-- site/
```

```yaml
applications:
  - name: mkdocsgo-example

    # The assembled directory, not the repository root.
    path: ../../dist/cf

    buildpacks:
      - binary_buildpack
    stack: cflinuxfs4

    # No -http: the server reads $PORT, which Cloud Foundry assigns per
    # instance. Nothing has to expand a variable, and nothing is hard-coded.
    command: ./mkdocsgo -mode site+mcp -project .

    instances: 2
    memory: 128M
    disk_quota: 512M

    health-check-type: http
    health-check-http-endpoint: /healthz
    health-check-invocation-timeout: 5

    routes:
      - route: docs.apps.example.com
```

```bash
cf push -f deploy/cf/manifest-buildpack.yml
```

Three things worth knowing:

- **`binary_buildpack`, not `go_buildpack`.** The Go buildpack would compile
  from source on the foundation, which means the Go toolchain, the module
  cache and network access to a proxy during staging - to produce a binary
  that a release already published, reproducibly, with a checksum.
- **The binary must be `linux/amd64`** (or whatever the foundation's cells
  are). Download it from the release rather than building it on a developer's
  Mac.
- **`chmod +x` survives the push**, but only if the file is executable when
  you assemble the directory. Set it in the packaging step; a non-executable
  binary fails at start with a permission error and no other clue.

### Why not the staticfile buildpack

`staticfile_buildpack` is the obvious CF answer for a built MkDocs site, and it
works - it runs nginx over `site/`. It is also exactly what mkdocsgo replaces:
it serves HTML and nothing else, so there is no `/mcp`, no `/healthz`, and the
cache and security headers come from an `nginx.conf` fragment you maintain per
application. Use it if you only ever needed a static site and no agent will
read this documentation. See [COMPARISON.md](./COMPARISON.md).

### Sizing

`memory: 128M` is comfortable. The process holds the gzipped copies of the site
in memory - 1.6 MB for a 77-file, 6.9 MB site - plus the section index, plus
the Go runtime. 64M works for a small site; measure with `cf app <name>` before
trimming.

Two instances for anything with a route people depend on: the ETags are content
hashes, so two replicas of the same build agree on them and a client that
reaches a different instance does not re-download.

---

## 3. Kubernetes

Nothing unusual: it is a stateless, read-only process with no volumes, no
secrets and no writes.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mkdocsgo-example
  labels:
    app.kubernetes.io/name: mkdocsgo-example
spec:
  replicas: 2
  selector:
    matchLabels:
      app.kubernetes.io/name: mkdocsgo-example
  template:
    metadata:
      labels:
        app.kubernetes.io/name: mkdocsgo-example
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532          # the distroless "nonroot" user
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: docs
          image: ghcr.io/kinjelom/mkdocsgo-example:0.1.0
          args: ["-mode", "site+mcp", "-project", "/project", "-http", "0.0.0.0:8080"]
          ports:
            - name: http
              containerPort: 8080
          # Everything is read at startup and never written. A read-only root
          # filesystem is free here, and there is no temp directory to grant.
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources:
            requests: {cpu: 10m,  memory: 64Mi}
            limits:   {cpu: 500m, memory: 128Mi}
          readinessProbe:
            httpGet: {path: /healthz, port: http}
            initialDelaySeconds: 1
            periodSeconds: 5
          livenessProbe:
            httpGet: {path: /healthz, port: http}
            initialDelaySeconds: 5
            periodSeconds: 20
```

The complete set - Deployment, Service, Ingress - is in the example repository
under `deploy/k8s/`.

Notes that are easy to get wrong:

- **`readOnlyRootFilesystem: true` is safe.** The process opens no file for
  writing and needs no scratch space. If a probe ever fails with a read-only
  error, something else changed.
- **Probe `/healthz`, not `/`.** `/` is a large response to fetch every five
  seconds, and an HTTP 200 from it proves nothing extra.
- **Startup is well under a second**, including hashing and compressing the
  site, so `initialDelaySeconds` does not need to be generous. A very large
  site is the exception - measure it.
- **No sticky sessions for `/mcp`.** The MCP endpoint is stateless: no
  `Mcp-Session-Id`, so any replica answers any request and the Service needs no
  affinity.
- **`args:` replaces the image's `CMD`**, not its `ENTRYPOINT`. Keep the
  entrypoint as the binary and pass flags here.

### Exposing it

```yaml
apiVersion: v1
kind: Service
metadata:
  name: mkdocsgo-example
spec:
  selector:
    app.kubernetes.io/name: mkdocsgo-example
  ports:
    - name: http
      port: 80
      targetPort: http
```

An Ingress in front terminates TLS. mkdocsgo speaks plain HTTP and has no
certificate handling: in every one of these three platforms something else
already terminates TLS - the CF router, the Ingress controller, your reverse
proxy - and doing it twice buys nothing.

---

## Choosing between the three

| You have                                  | Use                                                         |
|-------------------------------------------|-------------------------------------------------------------|
| Cloud Foundry and a registry it can reach | CF, Docker application                                      |
| Cloud Foundry and no registry             | CF, binary buildpack                                        |
| Kubernetes                                | Deployment + Service + Ingress                              |
| A single VM, or a developer's laptop      | `docker run`, or just the binary                            |
| Only a static file host (S3, Pages)       | you do not need mkdocsgo - but you do not get `/mcp` either |

## Security posture

**No authentication, by design.** Everything served is read-only and the
documentation is already published. If it is not public, put the server behind
what the platform provides - a Cloud Foundry route with an authenticating
gateway, an Ingress with OIDC, a reverse proxy.

The 2026-07-28 `Mcp-Method` and `Mcp-Name` headers let a gateway authorise
without parsing request bodies, so authentication can be added in front without
touching this code.

**`Origin` is validated on `/mcp`** - DNS-rebinding defence, not access
control. Requests with no `Origin` (every non-browser MCP client) pass,
loopback origins pass, anything else needs `-allow-origin`. The site itself is
not origin-guarded: it is a public website. Details in [MCP.md](./MCP.md).

**Panics are recovered per request.** That matters more in `site+mcp` than it
would in a single-purpose server: the two halves share a process, so an
unhandled failure in a search would otherwise stop the documentation being
served.

**Path traversal is not possible.** Site lookups resolve against a manifest
built at startup, and MCP lookups against the page set loaded from `docs_dir`.
Neither touches a caller-supplied filesystem path.

**Nothing is ever written.** No file is opened for writing, there is no scratch
directory and no state. That is what makes `readOnlyRootFilesystem: true` free
rather than brave.
