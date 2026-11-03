#!/usr/bin/env bash
# Verifies a multi-arch release image pushed by digest, before any tag
# points at it (Images workflow, build job, issue #10):
#
#   - the index has exactly the expected platforms;
#   - every platform carries an SBOM and a provenance attestation;
#   - the binary of every platform reports the release version.
#
# Usage: verify-image.sh <image@digest> <version> <platforms>
#   platforms  comma-separated, for example linux/amd64,linux/arm64
#
# Needs docker with buildx and, for foreign platforms, QEMU (binfmt).
set -euo pipefail

ref=$1 version=$2 platforms=$3
fail() { echo "::error title=Image verification::$*" >&2; exit 1; }

echo "== $ref"
docker buildx imagetools inspect "$ref"

index=$(docker buildx imagetools inspect "$ref" --format '{{json .Manifest}}')
digest=${ref##*@}
[ "$(jq -r .digest <<<"$index")" = "$digest" ] || fail "$ref resolves to $(jq -r .digest <<<"$index")"

# Attestation manifests are listed with platform unknown/unknown.
got=$(jq -r '[.manifests[] | select(.platform.os != "unknown") | "\(.platform.os)/\(.platform.architecture)"] | sort | join(",")' <<<"$index")
want=$(tr ',' '\n' <<<"$platforms" | sort | paste -sd, -)
[ "$got" = "$want" ] || fail "platforms are $got, want $want"
echo "platforms: $got"

sbom=$(docker buildx imagetools inspect "$ref" --format '{{json .SBOM}}')
provenance=$(docker buildx imagetools inspect "$ref" --format '{{json .Provenance}}')
for p in ${platforms//,/ }; do
  jq -e --arg p "$p" '.[$p].SPDX.packages | length > 0' <<<"$sbom" >/dev/null || fail "no SBOM for $p"
  jq -e --arg p "$p" '.[$p].SLSA | length > 0' <<<"$provenance" >/dev/null || fail "no provenance for $p"
  echo "attestations ($p): SBOM with $(jq --arg p "$p" '.[$p].SPDX.packages | length' <<<"$sbom") packages, provenance $(jq -r --arg p "$p" '.[$p].SLSA.predicateType // .[$p].SLSA.buildDefinition.buildType // "present"' <<<"$provenance")"
done

# Run each platform's own manifest, so the platform cannot silently fall back
# to another one.
for p in ${platforms//,/ }; do
  m=$(jq -r --arg os "${p%/*}" --arg arch "${p#*/}" \
    '.manifests[] | select(.platform.os == $os and .platform.architecture == $arch) | .digest' <<<"$index")
  out=$(docker run --rm --platform "$p" "${ref%@*}@$m" cli version)
  [ "$out" = "orglane $version" ] || fail "$p: 'orglane cli version' printed '$out', want 'orglane $version'"
  echo "version ($p, $m): $out"
done
echo "OK"
