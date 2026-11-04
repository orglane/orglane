#!/usr/bin/env bash
# Invocation boundary only; the real provider tests own all GitHub API calls.
set -euo pipefail

case "${1:-}" in
  tests|janitor) mode=$1 ;;
  *) echo '::error::Usage: github-live.sh tests|janitor'; exit 1 ;;
esac

if [[ "${ORGLANE_REQUIRE_GITHUB_LIVE:-}" != 1 ]]; then
  echo '::error::Required workflow runs must set ORGLANE_REQUIRE_GITHUB_LIVE=1.'
  exit 1
fi

missing=0
for name in \
  ORGLANE_TEST_GITHUB_ORG \
  ORGLANE_TEST_GITHUB_APP_ID \
  ORGLANE_TEST_GITHUB_APP_CLIENT_ID \
  ORGLANE_TEST_GITHUB_INSTALLATION_ID \
  ORGLANE_TEST_GITHUB_USER_LOGIN \
  ORGLANE_TEST_GITHUB_USER_ID \
  ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY \
  ORGLANE_TEST_GITHUB_WEBHOOK_SECRET \
  ORGLANE_TEST_GITHUB_USER_TOKEN; do
  if [[ -z "${!name:-}" ]]; then
    echo "::error::Missing github-live environment configuration: $name (see docs/github-live.md)."
    missing=1
  fi
done
if [[ "$missing" == 1 ]]; then exit 1; fi

# go test succeeds when -run matches nothing. Check each exact entrypoint before
# any tests can mutate the organization, and propagate listing failures.
require_test() {
  local package=$1 name=$2 listing
  listing=$(go test -tags integration -list "^${name}$" "$package")
  if ! printf '%s\n' "$listing" | grep -qx "$name"; then
    echo "::error::Missing ${name} in ${package} (see docs/github-live.md); the live harness is required."
    exit 1
  fi
}

require_test ./providers/github TestGitHubLiveJanitor
if [[ "$mode" == janitor ]]; then
  exec go test -tags integration -count=1 -timeout 5m -v -run '^TestGitHubLiveJanitor$' ./providers/github
fi

# Mirror `make test-github-live` (the maintainer's local target): the provider
# live tests without the janitor, then the M2 acceptance scenario's GitHub part.
require_test ./core/internal/apps TestM2AcceptanceGitHubLive
go test -tags integration -count=1 -timeout 10m -v -skip '^TestGitHubLiveJanitor$' ./providers/github
exec go test -tags integration -count=1 -timeout 10m -v -run '^TestM2AcceptanceGitHubLive$' ./core/internal/apps
