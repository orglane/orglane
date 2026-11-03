#!/usr/bin/env bash
# Runs the OpenID Conformance Suite (https://gitlab.com/openid/conformance-suite)
# against a locally started Orglane, for the profile Orglane implements:
#
#   oidcc-config-certification-test-plan
#   oidcc-basic-certification-test-plan[server_metadata=discovery][client_registration=static_client]
#
# Everything runs in Docker (see compose.yaml): PostgreSQL, Orglane (a Linux
# build of this checkout) behind a TLS proxy as https://op.orglane.test, and
# the suite (server, nginx, MongoDB) from its published images. The script
# creates the first owner and two OIDC clients, writes the suite
# configuration and runs the plans with the suite's own runner. Results are
# exported to $CONFORMANCE_WORK/results (JSON) and results/html (HTML).
# Exit status is non-zero on any unexpected failure or warning.
#
# Runs in different work directories (for example in different worktrees)
# can run at the same time: each gets its own Compose project, named after
# the work directory, with its own containers, volumes and network (Docker
# picks the subnet). A second run in the same work directory is refused.
#
# Environment:
#   CONFORMANCE_WORK  work directory (default: .conformance in the repository)
#   KEEP=1            leave the containers running afterwards (the script
#                     prints the command that stops them)
set -euo pipefail

SUITE_VERSION=release-v5.3.4
SUITE_REPO=https://gitlab.com/openid/conformance-suite.git
OWNER_EMAIL=conformance@orglane.test
OWNER_PASSWORD=conformance-password-0123
SUITE_CALLBACK=https://localhost.emobix.co.uk:8443/test/a/orglane/callback
PLANS=(
	"oidcc-config-certification-test-plan"
	"oidcc-basic-certification-test-plan[server_metadata=discovery][client_registration=static_client]"
)

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DIR="$ROOT/tests/conformance"
export CONFORMANCE_WORK="${CONFORMANCE_WORK:-$ROOT/.conformance}"
log() { printf '\n==> %s\n' "$*"; }

command -v docker >/dev/null || { echo "error: Docker is required"; exit 1; }
command -v openssl >/dev/null || { echo "error: openssl is required"; exit 1; }
mkdir -p "$CONFORMANCE_WORK"
CONFORMANCE_WORK="$(cd "$CONFORMANCE_WORK" && pwd)"

# One Compose project per work directory: concurrent runs elsewhere are never
# touched, and the next run here removes what a killed run left behind.
PROJECT="orglane-conformance-$(printf '%s' "$CONFORMANCE_WORK" | cksum | cut -d' ' -f1)"
compose() { docker compose -p "$PROJECT" -f "$DIR/compose.yaml" "$@"; }

# The work directory (binary, certificates, suite configuration, results)
# belongs to one run at a time.
LOCK="$CONFORMANCE_WORK/.lock"
if ! mkdir "$LOCK" 2>/dev/null; then
	echo "error: another conformance run is using $CONFORMANCE_WORK"
	echo "(set CONFORMANCE_WORK to another directory, or remove $LOCK if no run is active)"
	exit 1
fi

cleanup() {
	status=$?
	trap - EXIT INT TERM
	if [ "$status" -ne 0 ]; then
		echo "Orglane log (last 50 lines):"
		compose logs --no-color --tail 50 orglane 2>/dev/null || true
	fi
	if [ -z "${KEEP:-}" ]; then
		compose --profile run down -v --remove-orphans >/dev/null 2>&1 || true
	else
		echo "Containers left running. Stop them with: docker compose -p $PROJECT down -v"
	fi
	rmdir "$LOCK" 2>/dev/null || true
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

log "Compose project $PROJECT, work directory $CONFORMANCE_WORK"
mkdir -p "$CONFORMANCE_WORK/certs" "$CONFORMANCE_WORK/results"

log "Building orglane for linux/$(docker version --format '{{.Server.Arch}}')"
(cd "$ROOT" && GOOS=linux GOARCH="$(docker version --format '{{.Server.Arch}}')" CGO_ENABLED=0 \
	go build -trimpath -ldflags "-X main.version=conformance" -o "$CONFORMANCE_WORK/orglane" ./core/cmd/orglane)

log "Fetching the suite's runner scripts ($SUITE_VERSION)"
if [ "$(git -C "$CONFORMANCE_WORK/suite" describe --tags 2>/dev/null || true)" != "$SUITE_VERSION" ]; then
	rm -rf "$CONFORMANCE_WORK/suite"
	git clone --quiet --depth 1 --branch "$SUITE_VERSION" "$SUITE_REPO" "$CONFORMANCE_WORK/suite"
fi

log "Creating a throwaway CA and a certificate for op.orglane.test"
certs="$CONFORMANCE_WORK/certs"
cat >"$certs/ca.cnf" <<'CNF'
[req]
distinguished_name = dn
[dn]
[v3_ca]
basicConstraints = critical, CA:true
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
[v3_leaf]
basicConstraints = critical, CA:false
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:op.orglane.test
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid, issuer
CNF
openssl req -x509 -new -nodes -newkey rsa:2048 -days 7 -subj "/CN=Orglane conformance CA" \
	-config "$certs/ca.cnf" -extensions v3_ca -keyout "$certs/ca.key" -out "$certs/ca.crt" 2>/dev/null
openssl req -new -nodes -newkey rsa:2048 -subj "/CN=op.orglane.test" -config "$certs/ca.cnf" \
	-keyout "$certs/op.key" -out "$certs/op.csr" 2>/dev/null
openssl x509 -req -days 7 -in "$certs/op.csr" -CA "$certs/ca.crt" -CAkey "$certs/ca.key" -CAcreateserial \
	-extfile "$certs/ca.cnf" -extensions v3_leaf -out "$certs/op.crt" 2>/dev/null
chmod 644 "$certs"/*.key

log "Starting PostgreSQL, Orglane, the TLS proxy and the conformance suite"
compose --profile run down -v --remove-orphans >/dev/null 2>&1 || true
# The proxy starts first: Orglane trusts X-Forwarded-For from the proxy's
# address only, and Docker assigns that address when the proxy joins the
# project's network.
compose up -d proxy
proxy_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$(compose ps -q proxy)")"
[ -n "$proxy_ip" ] || { echo "error: no address for the TLS proxy"; exit 1; }
export CONFORMANCE_TRUSTED_PROXY="$proxy_ip/32"
compose up -d --wait postgres orglane
compose up -d mongodb server nginx
compose --profile run build --quiet runner

log "Creating the owner and the OIDC clients"
token="$(compose exec -T orglane /orglane cli setup-token | grep -o 'ost_[A-Za-z0-9_-]*')"
for i in $(seq 1 30); do
	compose --profile run run --rm -T runner python3 /conformance/bootstrap.py "$token" "$OWNER_EMAIL" "$OWNER_PASSWORD" && break
	[ "$i" -eq 30 ] && { echo "error: the TLS proxy did not come up"; exit 1; }
	sleep 2
done
add_client() {
	compose exec -T orglane /orglane cli oidc-client add -id "$1" -name "Conformance $1" -refresh-tokens "${@:2}" \
		-redirect-uri "$SUITE_CALLBACK" -post-logout-redirect-uri "https://localhost.emobix.co.uk:8443/test/a/orglane/post_logout_redirect" |
		sed -n 's/^client_secret: //p'
}
CLIENT1_SECRET="$(add_client conformance-1)"
CLIENT2_SECRET="$(add_client conformance-2)"
CLIENT3_SECRET="$(add_client conformance-3 -auth-method client_secret_post)"

compose --profile run run --rm -T \
	-e OWNER_EMAIL="$OWNER_EMAIL" -e OWNER_PASSWORD="$OWNER_PASSWORD" \
	-e CLIENT1_ID=conformance-1 -e CLIENT1_SECRET="$CLIENT1_SECRET" \
	-e CLIENT2_ID=conformance-2 -e CLIENT2_SECRET="$CLIENT2_SECRET" \
	-e CLIENT3_ID=conformance-3 -e CLIENT3_SECRET="$CLIENT3_SECRET" \
	runner python3 /conformance/make_config.py

log "Waiting for the conformance suite"
for i in $(seq 1 90); do
	if compose --profile run run --rm -T runner python3 -c \
		"import httpx,sys; sys.exit(httpx.get('https://localhost.emobix.co.uk:8443/api/runner/available', verify=False, timeout=5).status_code != 200)" 2>/dev/null; then
		break
	fi
	[ "$i" -eq 90 ] && { echo "error: the conformance suite did not start"; exit 1; }
	sleep 5
done

log "Running: ${PLANS[*]}"
args=()
for plan in "${PLANS[@]}"; do
	args+=("$plan" /work/config.json)
done
rm -rf "$CONFORMANCE_WORK/results" && mkdir -p "$CONFORMANCE_WORK/results"
status=0
compose --profile run run --rm -T runner python3 scripts/run-test-plan.py \
	--expected-failures-file /conformance/expected-failures.json \
	--expected-skips-file /conformance/expected-skips.json \
	--export-dir /work/results \
	"${args[@]}" || status=$?

# The runner exports JSON only; add the HTML reports, also when a plan failed.
# Same file names as the JSON exports, hence the subdirectory.
log "Exporting the HTML reports"
compose --profile run run --rm -T runner python3 /conformance/export_html.py /work/results/html ||
	{ echo "error: the HTML export failed"; [ "$status" -ne 0 ] || status=1; }

if [ "$status" -ne 0 ]; then
	log "Conformance run FAILED (exit status $status). Results: $CONFORMANCE_WORK/results"
	exit "$status"
fi
log "Conformance plans passed. Results: $CONFORMANCE_WORK/results"
