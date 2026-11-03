#!/usr/bin/env bash
# Moves the image's aliases (latest, X.Y) to the exact versions computed by
# `ci release-aliases` (Images workflow, promote job, issue #10). It runs in a
# single concurrency group across all releases, right after the aliases were
# computed from the registry, so it never races another promotion. Aliases
# already at the right digest are left alone.
#
# Usage: promote-aliases.sh <image> <aliases>
#   aliases  "<alias> <version>" lines, for example "latest 0.2.0"
set -euo pipefail

image=$1 aliases=$2
fail() { echo "::error title=Alias promotion::$*" >&2; exit 1; }
digest() { docker buildx imagetools inspect "$1" --format '{{json .Manifest}}' 2>/dev/null | jq -r .digest; }

while read -r alias version; do
  [ -n "$alias" ] || continue
  want=$(digest "$image:$version") || fail "$image:$version not found"
  have=$(digest "$image:$alias" || true)
  if [ "$have" = "$want" ]; then
    echo "$alias: already $version ($want)"
    continue
  fi
  docker buildx imagetools create --tag "$image:$alias" "$image@$want"
  [ "$(digest "$image:$alias")" = "$want" ] || fail "$image:$alias does not point at $want after promotion"
  echo "$alias: ${have:-(none)} -> $version ($want)"
done <<<"$aliases"
