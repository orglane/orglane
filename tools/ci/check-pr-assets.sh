#!/bin/sh
# Check the index: ignore rules alone do not stop git add --force.
set -eu
cd "$(git rev-parse --show-toplevel)"
tracked=$(git ls-files -- .github/pr-assets docs/screenshots)
if [ -n "$tracked" ]; then
    printf '%s\n' 'error: PR review assets must be uploaded to GitHub, not committed:' "$tracked" >&2
    exit 1
fi
