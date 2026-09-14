#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# mkdocsgo - cross-compile the release matrix into dist/
#
# For every platform in RELEASE_PLATFORMS (release.conf) it produces one
# archive holding both binaries and the documentation, then one checksums file
# covering all of them:
#
#   dist/mkdocsgo_1.2.0_linux_amd64.tar.gz
#   dist/mkdocsgo_1.2.0_darwin_arm64.tar.gz
#   dist/mkdocsgo_1.2.0_windows_amd64.zip
#   dist/mkdocsgo_1.2.0_checksums.txt
#
# These are the files scripts/release.sh uploads to the GitHub release, and the
# files a documentation project's bootstrap script downloads and verifies. The
# names are part of that contract - a consumer builds the URL from them - so
# they are computed in one place, scripts/lib.sh.
#
# Files inside an archive sit at its root, not under a directory, so a consumer
# can extract just the one binary it wants:
#
#   tar -xzf mkdocsgo_1.2.0_linux_amd64.tar.gz mkdocsgo
#
# Usage:
#   scripts/dist.sh                     # version from git describe
#   scripts/dist.sh --version 1.2.0     # the version a release will carry
#   scripts/dist.sh --platforms "linux/amd64 linux/arm64"
# ---------------------------------------------------------------------------

. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

load_config

VERSION=""
PLATFORMS=""

while [ $# -gt 0 ]; do
  case "$1" in
    --version)   VERSION="${2:?--version needs a value}"; shift ;;
    --platforms) PLATFORMS="${2:?--platforms needs a value}"; shift ;;
    -h|--help)   usage "${BASH_SOURCE[0]}"; exit 0 ;;
    *)           die "unknown argument: $1" ;;
  esac
  shift
done

need_cmd go "install it from https://go.dev/dl/"
need_cmd tar
[ -n "$VERSION" ]   || VERSION="$(describe_version)"
[ -n "$PLATFORMS" ] || PLATFORMS="$RELEASE_PLATFORMS"

cd "$REPO_ROOT"

step "Packaging $BINARY_NAME $VERSION"
log "platforms  : $PLATFORMS"
log "output     : $DIST_DIR"

# A stale archive from an earlier attempt must not be uploaded alongside the
# current one, so the version's own files are cleared rather than the whole
# directory - which may hold other versions someone still wants.
mkdir -p "$DIST_DIR"
rm -f "$DIST_DIR/${BINARY_NAME}_${VERSION}_"*

# Documentation shipped inside every archive: everything a consumer of the
# binary might want, and nothing that only concerns maintaining it - so
# RELEASING.md is deliberately absent. A file that does not exist is skipped
# rather than failing the build.
DOC_FILES=(
  README.md
  SERVING.md
  MCP.md
  ARCHITECTURE.md
  DEPLOYMENT.md
  COMPARISON.md
  CHANGELOG.md
  LICENSE
)

stage_docs() {
  local target="$1" f
  for f in "${DOC_FILES[@]}"; do
    [ -f "$REPO_ROOT/$f" ] && cp "$REPO_ROOT/$f" "$target/"
  done
  return 0
}

# zip is not installed everywhere; Python's zipfile is, wherever MkDocs is.
make_zip() {
  local archive="$1" dir="$2"
  if command -v zip >/dev/null 2>&1; then
    ( cd "$dir" && zip -q -r -X "$archive" . )
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c '
import os, sys, zipfile
archive, root = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as z:
    for base, _, files in os.walk(root):
        for name in sorted(files):
            full = os.path.join(base, name)
            z.write(full, os.path.relpath(full, root))
' "$archive" "$dir"
  else
    die "cannot build a zip archive: install zip, or python3"
  fi
}

built=0
for platform in $PLATFORMS; do
  goos="${platform%%/*}"
  goarch="${platform##*/}"
  [ "$goos" != "$platform" ] || die "malformed platform '$platform' - expected GOOS/GOARCH"

  base="$(archive_basename "$VERSION" "$goos" "$goarch")"
  ext="$(archive_extension "$goos")"
  staging="$DIST_DIR/.stage-$base"

  rm -rf "$staging"
  mkdir -p "$staging"

  "$REPO_ROOT/scripts/build.sh" \
      --version "$VERSION" --os "$goos" --arch "$goarch" --out "$staging" \
      >/dev/null

  stage_docs "$staging"

  case "$ext" in
    zip)    make_zip "$DIST_DIR/$base.$ext" "$staging" ;;
    # Archived from inside the staging directory, so entries are named
    # `mkdocsgo` and not `./mkdocsgo` - that is what makes
    # `tar -xzf archive.tar.gz mkdocsgo` extract the one file a consumer wants.
    #
    # --sort=name and a fixed mtime would make this byte-reproducible on GNU
    # tar, but BSD tar on macOS has neither; the binaries inside are
    # reproducible, which is what the checksums cover.
    tar.gz) ( cd "$staging" && tar -czf "$DIST_DIR/$base.$ext" -- * ) ;;
  esac

  rm -rf "$staging"
  ok "$(basename "$DIST_DIR/$base.$ext")  ($(du -h "$DIST_DIR/$base.$ext" | cut -f1))"
  built=$((built + 1))
done

[ "$built" -gt 0 ] || die "no platforms built - is RELEASE_PLATFORMS empty?"

step "Checksums"
checksums="$(checksums_name "$VERSION")"
( cd "$DIST_DIR" && sha256_of "${BINARY_NAME}_${VERSION}_"*.tar.gz "${BINARY_NAME}_${VERSION}_"*.zip 2>/dev/null > "$checksums" ) || true
[ -s "$DIST_DIR/$checksums" ] || die "checksum file is empty: $DIST_DIR/$checksums"
cat "$DIST_DIR/$checksums"

echo
ok "$built archive(s) in $DIST_DIR"
