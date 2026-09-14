# Releasing

One command turns a clean checkout into a published version:

```bash
scripts/release.sh patch
```

That produces an annotated tag, five platform archives with SHA-256 checksums,
a multi-tagged container image, and a GitHub release with the archives attached
and notes taken from the changelog.

## The version has one home

There is no `VERSION` file. **The released version is the git tag**, and
`scripts/release.sh` computes the next one from the highest existing `vX.Y.Z`.

This is not a stylistic preference. A version in a file can be bumped and not
tagged, tagged and not bumped, or bumped twice; every one of those has to be
detected and reconciled somewhere. A version that exists only as a tag cannot
disagree with itself, and `git describe` gives every intermediate build an
unambiguous name - `1.2.0-3-gabc1234`, or `-dirty` - that can never be mistaken
for a release.

The same rule reaches the binary: `-ldflags -X main.version=` bakes it in, so
`mkdocsgo -version` reports what it was built from rather than what someone
remembered to write down.

## The scripts

Each does one thing and can be run alone. `release.sh` is the orchestration.

| Script               | Does                                             | Run it directly when                     |
|----------------------|--------------------------------------------------|------------------------------------------|
| `scripts/test.sh`    | gofmt, `go vet`, `go test`                       | always, before pushing                   |
| `scripts/build.sh`   | host binaries into `bin/`                        | developing                               |
| `scripts/dist.sh`    | cross-compiled archives + checksums into `dist/` | you want the artefacts without a release |
| `scripts/image.sh`   | the container image, optionally pushed           | testing the image                        |
| `scripts/release.sh` | all of the above, plus tag, push and publish     | releasing                                |

Configuration - GitHub coordinates, registry, platforms, base images - is in
[`release.conf`](./release.conf), parsed and never sourced, with the
environment beating the file. A fork publishes elsewhere without a commit:

```bash
IMAGE_NAMESPACE=acme GITHUB_OWNER=acme scripts/release.sh patch
```

## What `release.sh` does, in order

```
1. preflight   clean tree, on DEFAULT_BRANCH, tag free, required tools present
2. test        scripts/test.sh
3. changelog   [Unreleased] becomes [X.Y.Z] - <today>, a fresh [Unreleased] opens
4. package     scripts/dist.sh  - five archives, one checksums file
5. image       scripts/image.sh - built and its version verified by running it
6. tag         commit the changelog, annotate vX.Y.Z with the release notes
7. publish     push branch + tag, push the image, gh release create + upload
```

**Nothing leaves the machine before step 7.** Everything that can fail - a
failing test, a broken cross-compile, a missing Docker daemon, an unauthenticated
`gh` - has already failed by then. A half-published release is not a state a
failure can put you in.

Step 1 checks the tools for the steps you actually asked for: `--no-publish`
does not require `gh`, `--no-image` does not require Docker.

If step 7 fails after the tag was pushed, the script says how to undo it rather
than leaving you to work it out:

```
WARN publishing failed after the tag was created
WARN to undo locally:  git tag -d v1.2.1
WARN to undo remotely: git push --delete origin v1.2.1
```

## The changelog is the release notes

`CHANGELOG.md` has an `## [Unreleased]` section. At release time it is renamed
to the version, dated, and a new empty `[Unreleased]` is opened above it. That
closed section becomes both the annotated tag's message and the GitHub release
body.

So the notes are what people wrote while doing the work, not a list of commit
subjects assembled at the end. Write the entry in the same commit as the
change.

If there is no `CHANGELOG.md`, or no `[Unreleased]` section, the script warns
and falls back to `gh release create --generate-notes`.

> The tag is written with `--cleanup=verbatim`. Git's default strips every line
> starting with `#`, which would silently delete the changelog's own Markdown
> headings from the tag message.

## Artefacts

```
dist/mkdocsgo_1.2.0_linux_amd64.tar.gz
dist/mkdocsgo_1.2.0_linux_arm64.tar.gz
dist/mkdocsgo_1.2.0_darwin_amd64.tar.gz
dist/mkdocsgo_1.2.0_darwin_arm64.tar.gz
dist/mkdocsgo_1.2.0_windows_amd64.zip
dist/mkdocsgo_1.2.0_checksums.txt
```

The names are a contract, not a convenience: a consuming project builds the
download URL from them. That is why they are computed in one place,
`scripts/lib.sh`, and why the example repository's bootstrap script can find
any version without being told the layout.

Each archive holds both binaries and the documentation, at the archive root -
so a consumer can take only what it needs:

```bash
tar -xzf mkdocsgo_1.2.0_linux_amd64.tar.gz mkdocsgo
```

`CGO_ENABLED=0` means every platform cross-compiles from any host without a
toolchain, and the result is static - it runs on distroless, on scratch, and on
any Linux regardless of libc. `-trimpath` keeps the build machine's paths out
of the binary, so two people building the same commit get the same bytes.

## The container image

`scripts/image.sh` builds `ghcr.io/kinjelom/mkdocsgo:<version>` and, unless
`IMAGE_LATEST_TAG` is `none`, `:latest` beside it. After a local build it runs
the image's own `-version` - the image is distroless, so the binary is the only
thing that can be asked what it is, and this also proves the entrypoint works.

`--multi-arch` builds every platform in `IMAGE_PLATFORMS` as one manifest list
through buildx. A manifest list cannot live in the local image store, so
`--multi-arch` implies `--push`.

Credentials come from the environment and go in on stdin:
`REGISTRY_USER`/`REGISTRY_PASSWORD`, or `GITHUB_TOKEN` for ghcr.io. A password
passed as an argument would be visible in the process list of every other user
on the machine.

Pushing to ghcr.io needs a token with the **`write:packages`** scope. The token
`gh auth login` mints does not have it - `gh` asks for `repo`, `read:org` and
`gist` only - so `GITHUB_TOKEN=$(gh auth token)` authenticates `gh` fine and
still fails at `docker push` with a 403. Add the scope once:

```bash
gh auth refresh -s write:packages
export GITHUB_TOKEN=$(gh auth token)
```

`scripts/release.sh` checks for this in its preflight, before anything is
pushed, and refuses to start without it. Pass `--no-image` to release the
binaries alone.

## The first release

There are no tags yet, so the script treats the current version as `0.0.0`.
Either of these produces `v0.1.0`:

```bash
scripts/release.sh minor        # 0.0.0 -> 0.1.0
scripts/release.sh 0.1.0        # the same thing, said explicitly
```

Before running it, once:

1. Commit everything. The tree must be clean and the branch must be `main`
   (`DEFAULT_BRANCH` in `release.conf`).
2. Push `main` to GitHub, so `gh` has a repository to attach a release to.
3. Authenticate: `gh auth login`, and - if the image is going to ghcr.io -
   `gh auth refresh -s write:packages` followed by
   `export GITHUB_TOKEN=$(gh auth token)`. See [The container image](#the-container-image) for why
   the extra scope is needed.

Then check it would work before it happens:

```bash
scripts/release.sh 0.1.0 --dry-run
```

### If you would rather publish by hand

The packaging step stands alone. This builds the six files and nothing else:

```bash
scripts/dist.sh --version 0.1.0
ls dist/
```

```
mkdocsgo_0.1.0_linux_amd64.tar.gz
mkdocsgo_0.1.0_linux_arm64.tar.gz
mkdocsgo_0.1.0_darwin_amd64.tar.gz
mkdocsgo_0.1.0_darwin_arm64.tar.gz
mkdocsgo_0.1.0_windows_amd64.zip
mkdocsgo_0.1.0_checksums.txt
```

Tag and push them yourself, then attach all six to the GitHub release:

```bash
git tag -a v0.1.0 -m "Release 0.1.0"
git push origin main --follow-tags
```

`scripts/release.sh 0.1.0 --no-publish` does the same thing with the changelog
promoted and the tag annotated from it, stopping before anything is pushed.

> Attach the checksums file too, not only the archives. A consuming project's
> bootstrap script downloads it to verify what it just fetched, which is the
> whole reason the download can be trusted.

## Before releasing

```bash
scripts/release.sh minor --dry-run
```

Runs the tests, builds every archive and the image, prints the exact plan - and
changes no tracked file, creates no tag, pushes nothing. Use it to check that a
release would work before deciding it should happen.

## Useful variations

```bash
scripts/release.sh patch                    # 1.2.0 -> 1.2.1
scripts/release.sh minor                    # 1.2.0 -> 1.3.0
scripts/release.sh major                    # 1.2.0 -> 2.0.0
scripts/release.sh 2.0.0-rc.1               # an exact version, pre-release allowed
scripts/release.sh patch --no-image         # skip the container image
scripts/release.sh patch --no-publish       # tag locally, push nothing
scripts/release.sh patch --allow-branch     # release from a branch that is not main
scripts/release.sh patch --yes              # no confirmation prompt
```

## Requirements

| Step              | Needs                                                                    |
|-------------------|--------------------------------------------------------------------------|
| test, build, dist | Go 1.25+ (`GOTOOLCHAIN=auto` fetches it if yours is older)               |
| image             | Docker or podman; buildx for `--multi-arch`                              |
| image push        | a registry login, or `GITHUB_TOKEN` with `write:packages` for ghcr.io    |
| publish           | `git`, [`gh`](https://cli.github.com/) authenticated, an `origin` remote |

## Wiring it into CI

The scripts are the interface, so CI is a thin wrapper. Two jobs cover it:

```bash
# on every push and pull request
scripts/test.sh

# on a manual trigger, with the bump as the input
scripts/release.sh "${BUMP}" --yes
```

The release job needs `GITHUB_TOKEN` - `gh` and `docker login ghcr.io` both
read it - and a checkout with full history (`fetch-depth: 0`), because the next
version is computed from the existing tags.

Drive it from a manual trigger rather than from a tag push: `release.sh`
*creates* the tag, so a job that fires on a tag would find it already there and
refuse. Something has to decide whether a change is a patch or a minor, and
that decision is not in the commits.

Keeping the logic in the scripts rather than in a workflow file is what lets a
release be reproduced - and debugged - on a laptop, exactly as CI would run
it.
