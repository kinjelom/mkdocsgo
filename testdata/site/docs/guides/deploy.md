# Deploying

## Rolling updates

A rolling deployment keeps the old instances serving until the new ones are
healthy, so an update causes no downtime.

```bash
# this comment must not be read as a heading
deploy --strategy rolling
```

## IMAGE_VERSION

Bump `IMAGE_VERSION` in image.conf before every release.
