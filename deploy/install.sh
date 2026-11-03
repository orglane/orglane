#!/bin/sh
# Orglane installer: sets up the Compose bundle (Traefik, the Orglane core,
# PostgreSQL) on this machine and starts it.
#
#   curl -fsSL https://orglane.app/install.sh | sh
#   curl -fsSL https://orglane.app/install.sh | sh -s -- --mode demo --yes
#
# Commands:
#
#   install.sh [install] [options]   install, or upgrade/repair an existing install
#   install.sh status    [options]   show the state of an install
#   install.sh uninstall [options]   stop and remove the containers (keeps data;
#                                    --purge deletes it)
#
# Run `install.sh --help` for the options. Everything the installer writes
# lives in the data directory (default /var/lib/orglane on Linux, ~/.orglane
# on macOS): the Compose bundle, the settings and secrets in .env (mode 0600)
# and the data itself. See deploy/README.md.
#
# POSIX sh: it must run under dash, busybox sh and bash. The whole script is
# read before anything runs (main is called on the last line), so a download
# cut short does nothing.

set -eu
umask 077

INSTALLER_VERSION=1
REPO=${ORGLANE_INSTALL_REPO:-orglane/orglane}
IMAGE_REPO=${ORGLANE_INSTALL_IMAGE_REPO:-ghcr.io/orglane/orglane}
# Minimum versions: `docker compose up --wait` and `env_file` with `path:`
# need Compose 2.24, healthcheck start_interval needs Docker Engine 25.
MIN_COMPOSE=2.24.0
MIN_ENGINE=25.0.0
LE_PRODUCTION=https://acme-v02.api.letsencrypt.org/directory
LE_STAGING=https://acme-staging-v02.api.letsencrypt.org/directory
# Public echo services for the server's own address (domain mode checks).
# --public-ip skips them.
IP_ECHO_V4=${ORGLANE_INSTALL_IP_ECHO_V4:-https://ipv4.icanhazip.com}
IP_ECHO_V6=${ORGLANE_INSTALL_IP_ECHO_V6:-https://ipv6.icanhazip.com}

# --- Output ------------------------------------------------------------------

if [ -t 2 ] && [ -z "${NO_COLOR:-}" ]; then
	C_BOLD=$(printf '\033[1m') C_RED=$(printf '\033[31m') C_YELLOW=$(printf '\033[33m')
	C_GREEN=$(printf '\033[32m') C_RESET=$(printf '\033[0m')
else
	C_BOLD='' C_RED='' C_YELLOW='' C_GREEN='' C_RESET=''
fi

say() { printf '%s\n' "$*" >&2; }
step() { printf '\n%s==> %s%s\n' "$C_BOLD" "$*" "$C_RESET" >&2; }
ok() { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$*" >&2; }
warn() { printf '  %s! %s%s\n' "$C_YELLOW" "$*" "$C_RESET" >&2; }
die() {
	printf '\n%serror:%s %s\n' "$C_RED" "$C_RESET" "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Orglane installer

Usage:
  install.sh [install] [options]   install, or upgrade/repair an existing install
  install.sh status [options]      show the state of an install
  install.sh uninstall [options]   remove the containers; --purge also deletes the data

Options:
  --mode <mode>            demo    try Orglane on this computer with the "Acme
                                   Robotics" sample company (http://localhost)
                           local   an empty instance on this computer (http://localhost)
                           domain  production on your own domain, HTTPS with
                                   Let's Encrypt (Linux servers)
  --domain <domain>        company base domain, e.g. acme.com (domain mode)
  --workspace-host <host>  workspace address, e.g. work.acme.com (default: the
                           base domain)
  --acme-email <email>     contact for the Let's Encrypt account (optional)
  --acme-ca <ca>           where certificates come from (domain mode):
                           production  Let's Encrypt (default)
                           staging     Let's Encrypt staging, untrusted test
                                       certificates without production limits
                           <url>       another ACME directory (https://...)
                           Re-runs keep the CA. Changing it deletes the stored
                           ACME account and certificates (traefik/acme.json),
                           so they are requested again from the new CA
  --acme-staging           same as --acme-ca staging
  --data-dir <path>        where everything lives (default /var/lib/orglane on
                           Linux, ~/.orglane on macOS)
  --version <version>      Orglane release to install, e.g. 0.1.0 (default: latest)
  --source <path>          install the Compose bundle from a checkout of the
                           repository (or its deploy/compose directory)
                           instead of downloading it; needs --version or --image
  --image <image>          core image to run instead of the release image
  --http-port <port>       HTTP port (default 80)
  --https-port <port>      HTTPS port (default 443, domain mode)
  --public-ip <ip>         this server's public IPv4 address for the DNS check
                           (default: asked from an echo service)
  --skip-checks            domain mode: skip the DNS and reachability checks
                           (for example behind split-horizon DNS)
  --install-docker         on Linux, install Docker Engine with the official
                           convenience script if it is missing
  --purge                  uninstall: also delete the data directory
  -y, --yes                don't ask: use the options and defaults
  -h, --help               show this help

Examples:
  curl -fsSL https://orglane.app/install.sh | sh
  curl -fsSL https://orglane.app/install.sh | sh -s -- --mode demo --yes
  curl -fsSL https://orglane.app/install.sh | sudo sh -s -- --mode domain \
      --domain acme.com --workspace-host work.acme.com --yes
  sh install.sh status
  sh install.sh uninstall --purge
EOF
}

# --- Prompts -------------------------------------------------------------------

# Answers are read from the terminal, also when the script itself comes from
# a pipe (curl | sh).
have_tty() {
	[ "$OPT_YES" = 0 ] || return 1
	if [ -t 0 ]; then return 0; fi
	(: </dev/tty) 2>/dev/null
}

read_answer() {
	if [ -t 0 ]; then
		IFS= read -r REPLY || REPLY=''
	else
		IFS= read -r REPLY </dev/tty || REPLY=''
	fi
}

# ask <question> <default>: sets ANSWER.
ask() {
	if ! have_tty; then
		ANSWER=$2
		return
	fi
	if [ -n "$2" ]; then
		printf '%s [%s]: ' "$1" "$2" >&2
	else
		printf '%s: ' "$1" >&2
	fi
	read_answer
	ANSWER=${REPLY:-$2}
}

# confirm <question> <default y|n>: true for yes. Without a terminal it uses
# --yes, otherwise the default.
confirm() {
	if [ "$OPT_YES" = 1 ]; then return 0; fi
	if ! have_tty; then
		[ "$2" = y ]
		return
	fi
	if [ "$2" = y ]; then
		printf '%s [Y/n]: ' "$1" >&2
	else
		printf '%s [y/N]: ' "$1" >&2
	fi
	read_answer
	case ${REPLY:-$2} in
	y | Y | yes | YES | Yes) return 0 ;;
	*) return 1 ;;
	esac
}

# --- Small helpers -------------------------------------------------------------

have() { command -v "$1" >/dev/null 2>&1; }

# version_ge <a> <b>: true if version a >= b (numeric x.y.z).
version_ge() {
	awk -v a="$1" -v b="$2" 'BEGIN {
		na = split(a, x, "."); nb = split(b, y, ".")
		for (i = 1; i <= 3; i++) {
			xi = x[i] + 0; yi = y[i] + 0
			if (xi > yi) exit 0
			if (xi < yi) exit 1
		}
		exit 0
	}'
}

# random_hex <bytes>: hex string from a CSPRNG.
random_hex() {
	if have openssl; then
		openssl rand -hex "$1"
	else
		od -An -tx1 -N"$1" /dev/urandom | tr -d ' \n'
		echo
	fi
}

# fetch <url>: download to stdout.
fetch() {
	if have curl; then
		curl -fsSL --retry 3 --connect-timeout 10 "$1"
	elif have wget; then
		wget -qO- "$1"
	else
		die "neither curl nor wget is installed"
	fi
}

valid_hostname() {
	printf '%s' "$1" | grep -Eq '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$'
}

valid_port() {
	case $1 in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

valid_ipv4() {
	printf '%s' "$1" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$'
}

# env_get <file> <key>: the value of KEY= in an env file (last one wins).
env_get() {
	sed -n "s/^$2=//p" "$1" | tail -n 1
}

# The containers' view of the data directory must be a real path: Docker
# Desktop creates missing bind-mount directories below a symlink inside its
# VM instead of on the Mac.
abs_path() {
	(cd "$1" && pwd -P)
}

# --- Platform and Docker -------------------------------------------------------

detect_platform() {
	OS=$(uname -s)
	case $(uname -m) in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	*) die "unsupported CPU architecture $(uname -m): Orglane images are built for amd64 and arm64" ;;
	esac
	case $OS in
	Linux) DEFAULT_DATA_DIR=/var/lib/orglane ;;
	Darwin) DEFAULT_DATA_DIR=${HOME:?}/.orglane ;;
	*) die "unsupported operating system $OS: Orglane runs on Linux (and on macOS with Docker Desktop for trying it)" ;;
	esac
}

install_docker() {
	say ""
	say "Docker Engine is not installed. Orglane runs in Docker containers."
	if [ "$OS" != Linux ]; then
		die "install Docker Desktop (https://docs.docker.com/desktop/) and start it, then run the installer again"
	fi
	if [ "$OPT_INSTALL_DOCKER" = 0 ]; then
		if [ "$OPT_YES" = 1 ] || ! have_tty; then
			die "install Docker Engine (https://docs.docker.com/engine/install/) or pass --install-docker to install it with the official convenience script"
		fi
		confirm "Install Docker Engine now with the official convenience script (https://get.docker.com)?" n ||
			die "install Docker Engine (https://docs.docker.com/engine/install/) and run the installer again"
	fi
	[ "$(id -u)" = 0 ] || die "installing Docker needs root: run the installer with sudo"
	step "Installing Docker Engine (https://get.docker.com)"
	fetch https://get.docker.com >"$WORK/get-docker.sh" || die "could not download https://get.docker.com"
	sh "$WORK/get-docker.sh" || die "the Docker convenience script failed"
}

check_docker() {
	if ! have docker; then
		install_docker
	fi
	if ! out=$(docker info --format '{{.ServerVersion}}' 2>&1); then
		case $out in
		*"permission denied"*)
			die "this user may not use Docker: run the installer with sudo, or add the user to the docker group" ;;
		*)
			die "Docker is installed but not running (start Docker Desktop or the docker service): $out" ;;
		esac
	fi
	ENGINE_VERSION=$out
	version_ge "$ENGINE_VERSION" "$MIN_ENGINE" ||
		die "Docker Engine $ENGINE_VERSION is too old: Orglane needs $MIN_ENGINE or newer"
	if ! COMPOSE_VERSION=$(docker compose version --short 2>/dev/null); then
		die "Docker Compose v2 is missing (the 'docker compose' plugin): see https://docs.docker.com/compose/install/"
	fi
	COMPOSE_VERSION=${COMPOSE_VERSION#v}
	version_ge "$COMPOSE_VERSION" "$MIN_COMPOSE" ||
		die "Docker Compose $COMPOSE_VERSION is too old: Orglane needs $MIN_COMPOSE or newer"
	ok "Docker Engine $ENGINE_VERSION, Compose $COMPOSE_VERSION"
}

# check_resources: warns below the recommended size and records the memory
# Docker can use (MEM_MB) for PostgreSQL sizing.
check_resources() {
	cpus=$(docker info --format '{{.NCPU}}' 2>/dev/null || echo 0)
	mem_bytes=$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo 0)
	MEM_MB=$((mem_bytes / 1048576))
	if [ "$MODE" = domain ]; then
		want_cpus=2 want_mem=3500 want_disk=10
	else
		want_cpus=2 want_mem=1800 want_disk=5
	fi
	# Free space where the data directory will be.
	probe=$DATA_DIR
	while [ ! -d "$probe" ]; do probe=$(dirname "$probe"); done
	disk_gb=$(df -Pk "$probe" | awk 'NR == 2 { printf "%d", $4 / 1048576 }')
	ok "$cpus CPUs, $MEM_MB MiB memory for Docker, $disk_gb GB free in $probe"
	[ "$cpus" -ge "$want_cpus" ] || warn "fewer CPUs than recommended ($want_cpus): Orglane will be slow"
	[ "$MEM_MB" -ge "$want_mem" ] || warn "less memory than recommended ($want_mem MiB): Orglane may run out of memory"
	[ "$disk_gb" -ge "$want_disk" ] || warn "less free disk space than recommended ($want_disk GB)"
}

# size_postgres: PostgreSQL settings from the memory Docker reports (ADR
# 0009). The rules are in deploy/README.md. A trial on a laptop (local, demo)
# counts at most 2 GB, so it doesn't claim a laptop's memory.
size_postgres() {
	mem=$MEM_MB
	if [ "$MODE" != domain ] && [ "$mem" -gt 2048 ]; then mem=2048; fi
	PG_MEM=$mem
	if [ "$mem" -lt 1024 ]; then
		PG_SHARED_BUFFERS=128 PG_CACHE=512 PG_WORK_MEM=4 PG_MAINT=64 PG_MAX_CONN=50
	else
		PG_SHARED_BUFFERS=$((mem / 4))
		[ "$PG_SHARED_BUFFERS" -le 8192 ] || PG_SHARED_BUFFERS=8192
		PG_CACHE=$((mem / 2))
		PG_MAINT=$((mem / 20))
		[ "$PG_MAINT" -le 1024 ] || PG_MAINT=1024
		[ "$PG_MAINT" -ge 64 ] || PG_MAINT=64
		if [ "$mem" -lt 3500 ]; then
			PG_WORK_MEM=4 PG_MAX_CONN=50
		elif [ "$mem" -lt 16000 ]; then
			PG_WORK_MEM=8 PG_MAX_CONN=100
		else
			PG_WORK_MEM=16 PG_MAX_CONN=100
		fi
	fi
}

# --- Installation state ----------------------------------------------------------

# dc: docker compose for the install in DATA_DIR. The settings come from the
# .env file there only: ORGLANE_* and COMPOSE_* variables of the caller's
# shell would override it.
dc() {
	(
		for v in $(env | sed -n -e 's/^\(ORGLANE_[A-Za-z0-9_]*\)=.*/\1/p' -e 's/^\(COMPOSE_[A-Za-z0-9_]*\)=.*/\1/p'); do
			unset "$v"
		done
		cd "$DATA_DIR" && docker compose "$@"
	)
}

# load_existing: reads the settings of an earlier install from DATA_DIR/.env.
load_existing() {
	ENV_FILE=$DATA_DIR/.env
	EXISTING=0
	[ -f "$ENV_FILE" ] || return 0
	EXISTING=1
	OLD_MODE=$(env_get "$ENV_FILE" ORGLANE_INSTALL_MODE)
	OLD_VERSION=$(env_get "$ENV_FILE" ORGLANE_VERSION)
	OLD_PG_PASSWORD=$(env_get "$ENV_FILE" ORGLANE_POSTGRES_PASSWORD)
	OLD_MASTER_SECRET=$(env_get "$ENV_FILE" ORGLANE_IDENTITY_MASTER_SECRET)
	OLD_DOMAIN=$(env_get "$ENV_FILE" ORGLANE_DOMAIN_BASE)
	OLD_WORKSPACE_HOST=$(env_get "$ENV_FILE" ORGLANE_DOMAIN_WORKSPACE_HOST)
	OLD_ACME_EMAIL=$(env_get "$ENV_FILE" ORGLANE_ACME_EMAIL)
	# Installs without the setting use the bundle's default, production.
	OLD_ACME_CA=$(env_get "$ENV_FILE" ORGLANE_ACME_CA_SERVER)
	OLD_ACME_CA=${OLD_ACME_CA:-$LE_PRODUCTION}
	OLD_HTTP_PORT=$(env_get "$ENV_FILE" ORGLANE_INSTALL_HTTP_PORT)
	OLD_HTTPS_PORT=$(env_get "$ENV_FILE" ORGLANE_INSTALL_HTTPS_PORT)
	OLD_HTTP_PUBLISH=$(env_get "$ENV_FILE" ORGLANE_HTTP_PUBLISH)
	OLD_HTTPS_PUBLISH=$(env_get "$ENV_FILE" ORGLANE_HTTPS_PUBLISH)
	[ -n "$OLD_MODE" ] || die "$ENV_FILE was not written by this installer: refusing to change it. Use another --data-dir"
	[ -n "$OLD_PG_PASSWORD" ] && [ -n "$OLD_MASTER_SECRET" ] ||
		die "$ENV_FILE lacks ORGLANE_POSTGRES_PASSWORD or ORGLANE_IDENTITY_MASTER_SECRET: restore them from a backup"
}

# workspace_url: the URL people open.
workspace_url() {
	case $1 in
	domain) printf 'https://%s\n' "$2" ;;
	*) printf 'http://%s\n' "$2" ;;
	esac
}

# --- Choosing the mode -------------------------------------------------------------

choose_mode() {
	if [ -n "$OPT_MODE" ]; then
		MODE=$OPT_MODE
		return
	fi
	if [ "$OPT_YES" = 1 ] || ! have_tty; then
		die "pass --mode demo, --mode local or --mode domain (no terminal to ask)"
	fi
	say ""
	say "How do you want to run Orglane?"
	say "  1) Try the demo on this computer: a sample company, one-click sign-in"
	say "  2) Use my domain: production with HTTPS (needs a Linux server with a public IP)"
	say "  3) An empty instance on this computer (http://localhost, no domain)"
	ask "Choose 1, 2 or 3" 1
	case $ANSWER in
	1 | demo) MODE=demo ;;
	2 | domain) MODE=domain ;;
	3 | local) MODE=local ;;
	*) die "unknown choice: $ANSWER" ;;
	esac
}

choose_domain() {
	DOMAIN=$OPT_DOMAIN
	WORKSPACE_HOST=$OPT_WORKSPACE_HOST
	ACME_EMAIL=$OPT_ACME_EMAIL
	if [ "$EXISTING" = 1 ]; then
		DOMAIN=${DOMAIN:-$OLD_DOMAIN}
		WORKSPACE_HOST=${WORKSPACE_HOST:-$OLD_WORKSPACE_HOST}
		ACME_EMAIL=${ACME_EMAIL:-$OLD_ACME_EMAIL}
	fi
	if [ -z "$DOMAIN" ]; then
		have_tty || die "pass --domain (the company base domain, e.g. acme.com)"
		say ""
		say "Orglane serves the workspace on one host and company services (such as git.acme.com)"
		say "on subdomains of the company base domain."
		ask "Company base domain (e.g. acme.com)" ""
		DOMAIN=$ANSWER
	fi
	DOMAIN=$(printf '%s' "$DOMAIN" | tr '[:upper:]' '[:lower:]')
	valid_hostname "$DOMAIN" || die "invalid domain: '$DOMAIN' (example: acme.com)"
	if [ -z "$WORKSPACE_HOST" ]; then
		ask "Workspace address (e.g. work.$DOMAIN)" "$DOMAIN"
		WORKSPACE_HOST=$ANSWER
	fi
	WORKSPACE_HOST=$(printf '%s' "$WORKSPACE_HOST" | tr '[:upper:]' '[:lower:]')
	valid_hostname "$WORKSPACE_HOST" || die "invalid workspace host: '$WORKSPACE_HOST' (example: work.$DOMAIN)"
	case $WORKSPACE_HOST in
	"$DOMAIN" | *."$DOMAIN") ;;
	*) die "the workspace host $WORKSPACE_HOST must be $DOMAIN or a subdomain of it" ;;
	esac
	if [ -z "$ACME_EMAIL" ] && [ "$EXISTING" = 0 ]; then
		ask "Email for Let's Encrypt notices (optional)" ""
		ACME_EMAIL=$ANSWER
	fi
	if [ -n "$ACME_EMAIL" ]; then
		printf '%s' "$ACME_EMAIL" | grep -Eq '^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$' ||
			die "invalid email: '$ACME_EMAIL'"
	fi
}

# choose_acme_ca: sets ACME_CA, the ACME directory Traefik uses. It is kept
# from an earlier install unless --acme-ca changes it; then ACME_CA_RESET=1,
# because Traefik keeps the old CA's account and certificates in acme.json.
choose_acme_ca() {
	ACME_CA_RESET=0
	case $OPT_ACME_CA in
	'') ACME_CA=${OLD_ACME_CA:-$LE_PRODUCTION} ;;
	production) ACME_CA=$LE_PRODUCTION ;;
	staging) ACME_CA=$LE_STAGING ;;
	*) ACME_CA=$OPT_ACME_CA ;;
	esac
	# No characters that Compose would interpolate in .env.
	printf '%s' "$ACME_CA" | grep -Eq '^https://[A-Za-z0-9._~:/?=&%-]+$' ||
		die "invalid --acme-ca '$OPT_ACME_CA': use production, staging or an https:// ACME directory URL"
	if [ "$ACME_CA" = "$LE_STAGING" ]; then
		warn "Let's Encrypt staging: browsers don't trust its certificates. Switch with --acme-ca production"
	fi
	if [ "$EXISTING" = 1 ] && [ "$ACME_CA" != "$OLD_ACME_CA" ]; then
		say ""
		say "  The ACME CA changes from $OLD_ACME_CA"
		say "  to $ACME_CA."
		confirm "Delete the stored ACME account and certificates (traefik/acme.json) and request them from the new CA?" y ||
			die "cancelled; nothing was changed"
		ACME_CA_RESET=1
	fi
}

# reset_acme: deletes Traefik's ACME account and certificates after a CA
# change. Traefik is stopped first: it rewrites acme.json from memory while
# it runs. The next start recreates it with the new CA.
reset_acme() {
	step "Switching the ACME CA"
	dc stop traefik >/dev/null 2>&1 || true
	acme=$DATA_DIR/traefik/acme.json
	if [ -e "$acme" ] && ! rm -f "$acme" 2>/dev/null; then
		# Written by Traefik as root.
		img=$(sed -n 's/^ *image: *\(traefik:[^ ]*\).*/\1/p' "$DATA_DIR/compose.yaml" | head -n 1)
		docker run --rm -v "$DATA_DIR/traefik:/data" --entrypoint rm "$img" -f /data/acme.json >/dev/null 2>&1 || true
	fi
	[ ! -e "$acme" ] || die "could not delete $acme"
	ok "deleted traefik/acme.json"
}

# --- The bundle ----------------------------------------------------------------------

# resolve_release: sets VERSION (without "v") and IMAGE.
resolve_release() {
	VERSION=${OPT_VERSION#v}
	if [ -n "$OPT_SOURCE" ] && [ -z "$OPT_VERSION" ] && [ -z "$OPT_IMAGE" ]; then
		die "--source needs --version (to run that release's image) or --image (e.g. orglane:dev from 'make image')"
	fi
	if [ -z "$VERSION" ] || [ "$VERSION" = latest ]; then
		if [ -n "$OPT_SOURCE" ]; then
			VERSION=custom
		else
			say "  looking up the latest release of $REPO"
			json=$(fetch "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null) ||
				die "no Orglane release is published yet (or GitHub is unreachable). Install from a checkout with --source <path> --image <image>"
			VERSION=$(printf '%s\n' "$json" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
			[ -n "$VERSION" ] || die "could not read the latest release from the GitHub API"
		fi
	fi
	printf '%s' "$VERSION" | grep -Eq '^([0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?|custom)$' ||
		die "invalid version: '$VERSION' (example: 0.1.0)"
	if [ -n "$OPT_IMAGE" ]; then
		IMAGE=$OPT_IMAGE
	else
		IMAGE=$IMAGE_REPO:$VERSION
	fi
}

# fetch_bundle: puts the Compose bundle (compose.yaml, modes/, pebble/) into
# $WORK/bundle.
fetch_bundle() {
	mkdir -p "$WORK/bundle"
	if [ -n "$OPT_SOURCE" ]; then
		if [ -f "$OPT_SOURCE/deploy/compose/compose.yaml" ]; then
			src=$OPT_SOURCE/deploy/compose
		elif [ -f "$OPT_SOURCE/compose.yaml" ] && [ -d "$OPT_SOURCE/modes" ]; then
			src=$OPT_SOURCE
		else
			die "--source $OPT_SOURCE is neither a checkout of the repository nor its deploy/compose directory"
		fi
		ok "Compose bundle from $src"
	else
		url=https://github.com/$REPO/archive/refs/tags/v$VERSION.tar.gz
		have tar || die "tar is not installed"
		fetch "$url" >"$WORK/source.tar.gz" || die "could not download the Compose bundle of $VERSION from $url"
		mkdir -p "$WORK/source"
		tar -xzf "$WORK/source.tar.gz" -C "$WORK/source" || die "could not unpack $url"
		src=$(find "$WORK/source" -path '*/deploy/compose/compose.yaml' | head -n 1)
		[ -n "$src" ] || die "the $VERSION source archive has no deploy/compose bundle"
		src=$(dirname "$src")
		ok "Compose bundle of Orglane $VERSION"
	fi
	cp "$src/compose.yaml" "$WORK/bundle/"
	cp -R "$src/modes" "$WORK/bundle/"
	if [ -d "$src/pebble" ]; then cp -R "$src/pebble" "$WORK/bundle/"; fi
}

# install_bundle: replaces the bundle files in DATA_DIR (data and .env stay).
install_bundle() {
	rm -rf "$DATA_DIR/modes" "$DATA_DIR/pebble"
	cp "$WORK/bundle/compose.yaml" "$DATA_DIR/compose.yaml"
	cp -R "$WORK/bundle/modes" "$DATA_DIR/modes"
	if [ -d "$WORK/bundle/pebble" ]; then cp -R "$WORK/bundle/pebble" "$DATA_DIR/pebble"; fi
	# Readable for the containers that mount files from it (Pebble overlay).
	chmod -R go+rX "$DATA_DIR/compose.yaml" "$DATA_DIR/modes"
	if [ -d "$DATA_DIR/pebble" ]; then chmod -R go+rX "$DATA_DIR/pebble"; fi
}

# passthrough <name>: an advanced setting taken from the environment on this
# run, else from the earlier install.
passthrough() {
	eval "val=\${$1:-}"
	if [ -z "$val" ] && [ "$EXISTING" = 1 ]; then val=$(env_get "$ENV_FILE" "$1"); fi
	if [ -n "$val" ]; then printf '%s=%s\n' "$1" "$val"; fi
}

write_env() {
	tmp=$DATA_DIR/.env.new
	: >"$tmp"
	chmod 600 "$tmp"
	{
		cat <<EOF
# Orglane settings, written by install.sh. Re-running install.sh keeps the
# secrets and the "Your settings" section at the end; it rewrites the rest.
# A setting in that section overrides the same one above (the last wins).
#
# Keep this file secret and back it up together with the data: without
# ORGLANE_IDENTITY_MASTER_SECRET the signing keys in the database can't be
# decrypted. Never change the secrets on a running instance.

ORGLANE_INSTALL_MODE=$MODE
ORGLANE_INSTALLER_VERSION=$INSTALLER_VERSION
ORGLANE_VERSION=$VERSION
ORGLANE_IMAGE=$IMAGE
ORGLANE_DATA_DIR=$DATA_DIR

ORGLANE_POSTGRES_PASSWORD=$PG_PASSWORD
ORGLANE_IDENTITY_MASTER_SECRET=$MASTER_SECRET

ORGLANE_INGRESS_MODE=$INGRESS_MODE
ORGLANE_DEMO_ENABLED=$DEMO_ENABLED
ORGLANE_DOMAIN_BASE=$DOMAIN
ORGLANE_DOMAIN_WORKSPACE_HOST=$WORKSPACE_HOST
ORGLANE_ACME_EMAIL=$ACME_EMAIL
EOF
		cat <<EOF
ORGLANE_ACME_CA_SERVER=$ACME_CA
ORGLANE_INSTALL_HTTP_PORT=$HTTP_PORT
ORGLANE_INSTALL_HTTPS_PORT=$HTTPS_PORT
ORGLANE_HTTP_PUBLISH=$HTTP_PUBLISH
ORGLANE_HTTPS_PUBLISH=$HTTPS_PUBLISH

# PostgreSQL, sized for $PG_MEM MiB of memory (see deploy/README.md).
ORGLANE_POSTGRES_SHARED_BUFFERS=${PG_SHARED_BUFFERS}MB
ORGLANE_POSTGRES_EFFECTIVE_CACHE_SIZE=${PG_CACHE}MB
ORGLANE_POSTGRES_WORK_MEM=${PG_WORK_MEM}MB
ORGLANE_POSTGRES_MAINTENANCE_WORK_MEM=${PG_MAINT}MB
ORGLANE_POSTGRES_MAX_CONNECTIONS=$PG_MAX_CONN
EOF
		extra=$(
			passthrough ORGLANE_EDGE_NETWORK_PREFIX
			passthrough COMPOSE_PROJECT_NAME
			passthrough COMPOSE_FILE
		)
		if [ -n "$extra" ]; then printf '\n%s\n' "$extra"; fi
		printf '\n# --- Your settings (kept by install.sh) ---\n'
		if [ "$EXISTING" = 1 ]; then
			sed -n '/^# --- Your settings/,$p' "$ENV_FILE" | sed 1d
		fi
	} >>"$tmp"
	mv "$tmp" "$DATA_DIR/.env"
	ENV_FILE=$DATA_DIR/.env
}

# --- Domain mode checks ------------------------------------------------------------

# resolve <A|AAAA> <host>: sets RESOLVED to the addresses DNS gives
# (space-separated, maybe none). Fails if no tool could get an answer, which
# is different from an answer without records.
resolve() {
	RESOLVED=''
	if have dig && out=$(dig +short +time=3 +tries=2 "$1" "$2" 2>/dev/null); then
		RESOLVED=$(printf '%s\n' "$out" | grep -E '^[0-9a-fA-F:.]+$' | tr '\n' ' ' || true)
		return 0
	fi
	if have getent; then
		if [ "$1" = A ]; then db=ahostsv4; else db=ahostsv6; fi
		rc=0
		out=$(getent "$db" "$2" 2>/dev/null) || rc=$?
		# 2: no such name.
		if [ "$rc" = 0 ] || [ "$rc" = 2 ]; then
			RESOLVED=$(printf '%s\n' "$out" | awk '$1 != "" && $1 !~ /^::ffff:/ { print $1 }' | sort -u | tr '\n' ' ')
			return 0
		fi
	fi
	if have host; then
		out=$(host -W 3 -t "$1" "$2" 2>&1 || true)
		case $out in
		*"timed out"* | *"no servers could be reached"* | *SERVFAIL*) ;;
		*)
			RESOLVED=$(printf '%s\n' "$out" | sed -n 's/.* has address //p; s/.* has IPv6 address //p' | tr '\n' ' ')
			return 0
			;;
		esac
	fi
	return 1
}

need_dns_tool() {
	have dig || have host || have getent ||
		die "no DNS lookup tool found (dig, host or getent): install dnsutils, or pass --skip-checks"
}

# public_ip <4|6>: this server's public address, or nothing.
public_ip() {
	if [ "$1" = 4 ] && [ -n "$OPT_PUBLIC_IP" ]; then
		printf '%s\n' "$OPT_PUBLIC_IP"
		return
	fi
	[ "$1" = 6 ] && [ -n "$OPT_PUBLIC_IP" ] && return 0
	if [ "$1" = 4 ]; then url=$IP_ECHO_V4; else url=$IP_ECHO_V6; fi
	if have curl; then
		curl "-$1" -fsS --max-time 10 "$url" 2>/dev/null | tr -d '[:space:]' || true
	elif have wget; then
		wget "-$1" -qO- -T 10 "$url" 2>/dev/null | tr -d '[:space:]' || true
	fi
}

show_dns_records() {
	say ""
	say "Create these DNS records at your DNS provider (or check they exist):"
	say ""
	printf '    %-32s %-5s %s\n' "$WORKSPACE_HOST" A "$PUBLIC_V4" >&2
	if [ -n "$PUBLIC_V6" ]; then printf '    %-32s %-5s %s\n' "$WORKSPACE_HOST" AAAA "$PUBLIC_V6" >&2; fi
	printf '    %-32s %-5s %s\n' "*.$DOMAIN" A "$PUBLIC_V4" >&2
	if [ -n "$PUBLIC_V6" ]; then printf '    %-32s %-5s %s\n' "*.$DOMAIN" AAAA "$PUBLIC_V6" >&2; fi
	say ""
	say "  The first is the workspace. The wildcard is for company services such as"
	say "  git.$DOMAIN; it is needed once you add one."
	if [ -z "$PUBLIC_V6" ]; then
		say "  This server has no public IPv6 address: don't create AAAA records for these names."
	fi
}

# check_dns: refuses (strict=1) or warns if the workspace host doesn't
# resolve to this server. Let's Encrypt validates every address in DNS and
# prefers IPv6, so a stale AAAA record fails issuance too.
check_dns() {
	strict=$1
	need_dns_tool
	PUBLIC_V4=$(public_ip 4)
	valid_ipv4 "$PUBLIC_V4" || die "could not find this server's public IPv4 address (asked $IP_ECHO_V4): pass --public-ip <address>"
	PUBLIC_V6=$(public_ip 6)
	case $PUBLIC_V6 in *:*) ;; *) PUBLIC_V6='' ;; esac
	ok "public address $PUBLIC_V4${PUBLIC_V6:+ and $PUBLIC_V6}"
	show_dns_records

	problems=''
	resolve A "$WORKSPACE_HOST" && resolve_ok=1 || resolve_ok=0
	a=$RESOLVED
	resolve AAAA "$WORKSPACE_HOST" || resolve_ok=0
	aaaa=$RESOLVED
	if [ "$resolve_ok" = 0 ]; then
		[ "$strict" = 1 ] || { warn "DNS: could not look up $WORKSPACE_HOST"; return 0; }
		die "could not look up $WORKSPACE_HOST in DNS (no answer from the resolvers), so no certificate was requested. Check this server's DNS settings (/etc/resolv.conf) and run the installer again"
	fi
	if [ -z "$a" ]; then
		problems="$WORKSPACE_HOST has no A record (want $PUBLIC_V4)"
	else
		for ip in $a; do
			[ "$ip" = "$PUBLIC_V4" ] || problems="$problems${problems:+; }$WORKSPACE_HOST resolves to $ip, not to this server ($PUBLIC_V4)"
		done
	fi
	for ip in $aaaa; do
		if [ -z "$PUBLIC_V6" ]; then
			problems="$problems${problems:+; }$WORKSPACE_HOST has an AAAA record ($ip), but this server has no public IPv6 address: delete the record"
		elif [ "$ip" != "$PUBLIC_V6" ]; then
			problems="$problems${problems:+; }$WORKSPACE_HOST has the AAAA record $ip, not this server ($PUBLIC_V6)"
		fi
	done
	if [ -n "$problems" ]; then
		if [ "$strict" = 1 ]; then
			die "DNS is not ready, so no certificate was requested (Let's Encrypt limits failed attempts): $problems.
Create the records above, wait until 'dig +short $WORKSPACE_HOST' shows $PUBLIC_V4 (DNS changes can take a few minutes up to the record's TTL), then run the installer again.
If the server's public address differs from $PUBLIC_V4, pass --public-ip."
		fi
		warn "DNS: $problems"
		return 0
	fi
	ok "$WORKSPACE_HOST resolves to this server"
	# A random name under the base domain tests the wildcard record.
	resolve A "orglane-dns-check-$(random_hex 4).$DOMAIN" || true
	case " $RESOLVED " in
	*" $PUBLIC_V4 "*) ok "*.$DOMAIN resolves to this server" ;;
	*) warn "*.$DOMAIN does not resolve to this server yet: create the wildcard record before adding company services" ;;
	esac
}

# check_reachability: starts a throwaway Traefik on ports 80 and 443 and
# fetches it through the workspace host's DNS name. This proves the ports
# are free and DNS leads here. It cannot see a cloud provider's firewall, so
# the summary reminds about it.
check_reachability() {
	traefik_image=$(sed -n 's/^ *image: *\(traefik:[^ ]*\).*/\1/p' "$WORK/bundle/compose.yaml" | head -n 1)
	[ -n "$traefik_image" ] || die "no Traefik image in the Compose bundle"
	say "  pulling $traefik_image"
	docker pull -q "$traefik_image" >/dev/null || die "could not pull $traefik_image"
	PROBE=orglane-install-probe-$$
	if ! out=$(docker run -d --rm --name "$PROBE" -p "$HTTP_PORT:80" -p "$HTTPS_PORT:443" "$traefik_image" \
		--global.checknewversion=false --global.sendanonymoususage=false \
		--entrypoints.web.address=:80 --entrypoints.websecure.address=:443 \
		--ping=true --ping.entrypoint=web 2>&1); then
		PROBE=''
		die "could not listen on ports $HTTP_PORT and $HTTPS_PORT: another program (a web server?) uses them. Stop it and run the installer again.
$out"
	fi
	i=0
	until [ "$(fetch_quiet "http://127.0.0.1:$HTTP_PORT/ping")" = OK ]; do
		i=$((i + 1))
		[ "$i" -lt 30 ] || { stop_probe; die "the reachability probe did not start"; }
		sleep 1
	done
	if [ "$(fetch_quiet "http://$WORKSPACE_HOST:$HTTP_PORT/ping")" != OK ]; then
		stop_probe
		die "http://$WORKSPACE_HOST:$HTTP_PORT is not reachable from this server, so no certificate was requested.
Open ports 80 and 443 (TCP) in the server's firewall (e.g. 'ufw allow 80,443/tcp') and in your hosting provider's firewall, then run the installer again.
Behind NAT without hairpin routing this check fails even when the ports are forwarded: then pass --skip-checks."
	fi
	if [ "$(http_code "http://$WORKSPACE_HOST:$HTTPS_PORT/")" != 404 ]; then
		stop_probe
		die "port $HTTPS_PORT of $WORKSPACE_HOST is not reachable from this server. Open ports 80 and 443 (TCP) in the firewalls, then run the installer again (--skip-checks skips this check)."
	fi
	stop_probe
	ok "ports $HTTP_PORT and $HTTPS_PORT of $WORKSPACE_HOST reach this server"
	if have ufw && ufw status 2>/dev/null | grep -q 'Status: active'; then
		ufw status 2>/dev/null | grep -Eq '(^| )(80|443)(/tcp)?( |,)|80,443' ||
			warn "ufw is active and may block ports 80 and 443 from outside: 'ufw allow 80,443/tcp'"
	fi
}

stop_probe() {
	if [ -n "${PROBE:-}" ]; then
		docker rm -f "$PROBE" >/dev/null 2>&1 || true
		PROBE=''
	fi
}

fetch_quiet() {
	if have curl; then
		curl -fsS --max-time 5 "$1" 2>/dev/null || true
	else
		wget -qO- -T 5 "$1" 2>/dev/null || true
	fi
}

http_code() {
	if have curl; then
		curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$1" 2>/dev/null || true
	else
		wget -S -O /dev/null -T 5 "$1" 2>&1 | awk '/^  HTTP\// { c = $2 } END { print c }'
	fi
}

# --- Starting and checking ------------------------------------------------------------

# published <port>: the host address Traefik's container port is published
# on, reachable from this machine.
published() {
	dc port traefik "$1" 2>/dev/null | head -n 1 | sed 's/^0\.0\.0\.0:/127.0.0.1:/; s/^\[::\]:/127.0.0.1:/'
}

# workspace_ready: the workspace answers /readyz through Traefik.
workspace_ready() {
	if [ "$INGRESS_MODE" = domain ]; then
		addr=$(published 443)
		[ -n "$addr" ] || return 1
		have curl || return 0
		curl -fsSk -o /dev/null --max-time 5 --resolve "$WORKSPACE_HOST:${addr##*:}:${addr%:*}" \
			"https://$WORKSPACE_HOST:${addr##*:}/readyz" 2>/dev/null
	else
		addr=$(published 80)
		[ -n "$addr" ] || return 1
		if have curl; then
			curl -fsS -o /dev/null --max-time 5 -H "Host: $WORKSPACE_HOST" "http://$addr/readyz" 2>/dev/null
		else
			wget -q -O /dev/null -T 5 --header "Host: $WORKSPACE_HOST" "http://$addr/readyz" 2>/dev/null
		fi
	fi
}

wait_ready() {
	i=0
	until workspace_ready; do
		i=$((i + 1))
		if [ "$i" -ge 90 ]; then
			die "the workspace did not become ready. See 'cd $DATA_DIR && docker compose logs'"
		fi
		sleep 2
	done
	ok "the workspace answers on $(workspace_url "$INGRESS_MODE" "$WORKSPACE_HOST")"
}

# wait_certificate: domain mode; waits until Traefik serves a certificate
# from the ACME CA instead of its default one, then checks it is trusted.
wait_certificate() {
	have openssl || { warn "openssl is missing: not checking the certificate"; return 0; }
	addr=$(published 443)
	i=0
	while :; do
		issuer=$(printf '' | openssl s_client -connect "$addr" -servername "$WORKSPACE_HOST" 2>/dev/null |
			openssl x509 -noout -issuer 2>/dev/null || true)
		case $issuer in
		'' | *"TRAEFIK DEFAULT CERT"*) ;;
		*) break ;;
		esac
		i=$((i + 1))
		if [ "$i" -ge 60 ]; then
			die "no certificate was issued for $WORKSPACE_HOST within 3 minutes. See 'cd $DATA_DIR && docker compose logs traefik'"
		fi
		sleep 3
	done
	ok "certificate issued (${issuer#issuer=})"
	if have curl && ! curl -fsS -o /dev/null --max-time 10 --resolve "$WORKSPACE_HOST:${addr##*:}:${addr%:*}" \
		"https://$WORKSPACE_HOST:${addr##*:}/readyz" 2>/dev/null; then
		warn "the certificate is not trusted by this machine (a test CA such as Let's Encrypt staging?)"
	fi
}

start_bundle() {
	step "Starting Orglane"
	log=$WORK/compose-up.log
	# The status of the compose command, not of tee.
	{
		dc up --detach --wait --wait-timeout 300 --remove-orphans 2>&1
		echo "$?" >"$WORK/compose-up.status"
	} | tee "$log" >&2
	if [ "$(cat "$WORK/compose-up.status")" != 0 ]; then
		if grep -Eq 'address already in use|port is already allocated' "$log"; then
			die "a port Orglane needs is in use by another program. Stop it, or choose another one with --http-port (local and demo modes)"
		fi
		die "the containers did not start. See 'cd $DATA_DIR && docker compose logs'"
	fi
}

# setup_token: a new one-time setup token for the owner, or nothing once
# setup is complete.
setup_token() {
	out=$(dc exec -T orglane /usr/local/bin/orglane cli setup-token 2>&1) || {
		case $out in
		*"already complete"*) return 0 ;;
		*) warn "could not create a setup token: $out" ;;
		esac
		return 0
	}
	printf '%s\n' "$out" | sed -n 's/^ *\(ost_[A-Za-z0-9_-]*\).*/\1/p' | head -n 1
}

# --- Commands -------------------------------------------------------------------------

cmd_install() {
	START=$(date +%s)
	step "Checking this machine"
	detect_platform
	ok "$OS $ARCH"
	DATA_DIR=${OPT_DATA_DIR:-$DEFAULT_DATA_DIR}
	case $DATA_DIR in
	/*) ;;
	*) DATA_DIR=$(pwd -P)/$DATA_DIR ;;
	esac
	if [ -d "$DATA_DIR" ]; then DATA_DIR=$(abs_path "$DATA_DIR"); fi
	check_docker
	load_existing

	if [ "$EXISTING" = 1 ]; then
		say ""
		say "Orglane is already installed in $DATA_DIR ($OLD_MODE mode, version $OLD_VERSION)."
		if [ -n "$OPT_MODE" ] && [ "$OPT_MODE" != "$OLD_MODE" ]; then
			die "this install runs in $OLD_MODE mode; changing it to $OPT_MODE is not supported. Remove it first with 'install.sh uninstall --purge' (deletes its data) or use another --data-dir"
		fi
		MODE=$OLD_MODE
		confirm "Upgrade or repair it? Secrets and data are kept" y || die "cancelled"
	else
		choose_mode
	fi
	case $MODE in
	demo | local) INGRESS_MODE=local ;;
	domain) INGRESS_MODE=domain ;;
	*) die "unknown mode '$MODE': use demo, local or domain" ;;
	esac
	if [ "$MODE" = domain ] && [ "$OS" != Linux ]; then
		warn "domain mode is meant for a Linux server with a public IP address; on $OS it is not supported"
	fi
	DEMO_ENABLED=false
	[ "$MODE" != demo ] || DEMO_ENABLED=true

	HTTP_PORT=${OPT_HTTP_PORT:-${OLD_HTTP_PORT:-80}}
	HTTPS_PORT=${OPT_HTTPS_PORT:-${OLD_HTTPS_PORT:-443}}
	valid_port "$HTTP_PORT" || die "invalid --http-port: $HTTP_PORT"
	valid_port "$HTTPS_PORT" || die "invalid --https-port: $HTTPS_PORT"
	if [ "$INGRESS_MODE" = domain ]; then
		choose_domain
		choose_acme_ca
		DEFAULT_HTTP_PUBLISH=$HTTP_PORT
		DEFAULT_HTTPS_PUBLISH=$HTTPS_PORT
		if [ "$HTTP_PORT" != 80 ] || [ "$HTTPS_PORT" != 443 ]; then
			warn "Let's Encrypt connects to ports 80 and 443: forward them to $HTTP_PORT and $HTTPS_PORT"
		fi
	else
		DOMAIN=localhost
		WORKSPACE_HOST=localhost
		if [ "$HTTP_PORT" != 80 ]; then WORKSPACE_HOST=localhost:$HTTP_PORT; fi
		ACME_EMAIL=''
		[ -z "$OPT_ACME_CA" ] || die "--acme-ca applies to domain mode only"
		ACME_CA=${OLD_ACME_CA:-$LE_PRODUCTION}
		ACME_CA_RESET=0
		# Local mode serves plain HTTP on localhost only; nothing needs 443
		# on the host, so it gets a random port.
		DEFAULT_HTTP_PUBLISH=127.0.0.1:$HTTP_PORT
		DEFAULT_HTTPS_PUBLISH=127.0.0.1:
	fi
	# Where Traefik is published: ORGLANE_HTTP(S)_PUBLISH from the
	# environment, else the earlier install's unless the ports change.
	HTTP_PUBLISH=${ORGLANE_HTTP_PUBLISH:-}
	HTTPS_PUBLISH=${ORGLANE_HTTPS_PUBLISH:-}
	if [ -z "$OPT_HTTP_PORT$OPT_HTTPS_PORT" ]; then
		HTTP_PUBLISH=${HTTP_PUBLISH:-$OLD_HTTP_PUBLISH}
		HTTPS_PUBLISH=${HTTPS_PUBLISH:-$OLD_HTTPS_PUBLISH}
	fi
	HTTP_PUBLISH=${HTTP_PUBLISH:-$DEFAULT_HTTP_PUBLISH}
	HTTPS_PUBLISH=${HTTPS_PUBLISH:-$DEFAULT_HTTPS_PUBLISH}

	check_resources
	size_postgres

	step "Getting Orglane"
	resolve_release
	if [ "$EXISTING" = 1 ] && [ "$OLD_VERSION" != "$VERSION" ]; then
		say "  upgrading from $OLD_VERSION to $VERSION"
	fi
	fetch_bundle
	ok "image $IMAGE"

	running=''
	if [ "$EXISTING" = 1 ]; then running=$(dc ps --quiet traefik 2>/dev/null || true); fi
	if [ "$INGRESS_MODE" = domain ]; then
		step "Checking DNS and reachability for $WORKSPACE_HOST"
		if [ "$OPT_SKIP_CHECKS" = 1 ]; then
			warn "skipped (--skip-checks): make sure $WORKSPACE_HOST points at this server and ports 80 and 443 are open"
		elif [ -n "$running" ]; then
			# Upgrade or repair: Traefik holds the ports, and refusing a
			# repair over DNS would not help.
			check_dns 0
		else
			check_dns 1
			check_reachability
		fi
	elif [ -z "$running" ] && [ "$HTTP_PUBLISH" = "$DEFAULT_HTTP_PUBLISH" ]; then
		code=$(http_code "http://127.0.0.1:$HTTP_PORT/")
		if [ -n "$code" ] && [ "$code" != 000 ]; then
			die "port $HTTP_PORT is in use by another program. Stop it, or choose another port with --http-port (e.g. --http-port 8080)"
		fi
	fi

	step "Writing $DATA_DIR"
	if [ "$EXISTING" = 1 ]; then
		PG_PASSWORD=$OLD_PG_PASSWORD
		MASTER_SECRET=$OLD_MASTER_SECRET
		ok "kept the existing secrets"
	else
		PG_PASSWORD=$(random_hex 24)
		MASTER_SECRET=$(random_hex 32)
		[ "${#PG_PASSWORD}" -eq 48 ] && [ "${#MASTER_SECRET}" -eq 64 ] || die "could not generate secrets"
		ok "generated the database password and the master secret"
	fi
	mkdir -p "$DATA_DIR" || die "could not create $DATA_DIR: run the installer as root (curl ... | sudo sh) or pass --data-dir"
	chmod 700 "$DATA_DIR"
	DATA_DIR=$(abs_path "$DATA_DIR")
	install_bundle
	write_env
	ok "Compose bundle and settings (.env, mode 600)"

	if [ "$ACME_CA_RESET" = 1 ]; then reset_acme; fi
	start_bundle
	wait_ready
	if [ "$INGRESS_MODE" = domain ]; then wait_certificate; fi

	URL=$(workspace_url "$INGRESS_MODE" "$WORKSPACE_HOST")
	ELAPSED=$(($(date +%s) - START))
	step "Orglane is running (ready in ${ELAPSED}s)"
	say ""
	say "  Open ${C_BOLD}$URL${C_RESET}"
	if [ "$MODE" = demo ]; then
		say "  and sign in as any person of the \"Acme Robotics\" demo company with one click."
		say "  Anyone who can open this address can do that: keep the demo on this computer."
	else
		token=$(setup_token)
		if [ -n "$token" ]; then
			say "  and create the owner account with this one-time setup token:"
			say ""
			say "    ${C_BOLD}$token${C_RESET}"
		else
			say "  and sign in (setup is complete)."
		fi
	fi
	say ""
	say "  Data, settings and secrets: $DATA_DIR (back up .env with the data)"
	say "  Status:     $INSTALLER status${OPT_DATA_DIR:+ --data-dir $DATA_DIR}"
	say "  Logs:       cd $DATA_DIR && docker compose logs -f"
	say "  Upgrade:    $INSTALLER${OPT_DATA_DIR:+ --data-dir $DATA_DIR}"
	say "  Uninstall:  $INSTALLER uninstall${OPT_DATA_DIR:+ --data-dir $DATA_DIR}"
	if [ "$INGRESS_MODE" = domain ]; then
		say ""
		say "  If $URL is unreachable from other networks, open ports 80 and 443 (TCP) in your"
		say "  hosting provider's firewall: the installer can only check from this server."
	fi
	printf '%s\n' "$URL"
}

locate_install() {
	detect_platform
	DATA_DIR=${OPT_DATA_DIR:-$DEFAULT_DATA_DIR}
	[ -f "$DATA_DIR/.env" ] || die "no Orglane install in $DATA_DIR (pass --data-dir if it is elsewhere)"
	DATA_DIR=$(abs_path "$DATA_DIR")
	have docker || die "docker is not installed"
	load_existing
	MODE=$OLD_MODE
	INGRESS_MODE=local
	[ "$MODE" != domain ] || INGRESS_MODE=domain
	WORKSPACE_HOST=$OLD_WORKSPACE_HOST
}

cmd_status() {
	locate_install
	say "Orglane $OLD_VERSION, $MODE mode, in $DATA_DIR"
	say "Workspace: $(workspace_url "$INGRESS_MODE" "$WORKSPACE_HOST")"
	say ""
	dc ps --format 'table {{.Service}}\t{{.State}}\t{{.Status}}' >&2 || die "docker compose failed"
	say ""
	if workspace_ready; then
		ok "the workspace is ready"
	else
		say "${C_RED}the workspace is not ready${C_RESET}: see 'cd $DATA_DIR && docker compose logs'"
		exit 1
	fi
}

cmd_uninstall() {
	locate_install
	if [ "$OPT_PURGE" = 1 ] && [ "$OPT_YES" = 0 ]; then
		have_tty || die "--purge deletes all data: confirm with --yes"
		say ""
		say "This deletes $DATA_DIR: the database, certificates and secrets. It can't be undone."
		ask "Type 'delete' to confirm" ""
		[ "$ANSWER" = delete ] || die "cancelled; nothing was changed"
	fi
	step "Removing the Orglane containers ($DATA_DIR)"
	dc down --remove-orphans --timeout 30 >&2 || die "docker compose down failed"
	ok "containers and networks removed"
	if [ "$OPT_PURGE" = 0 ]; then
		say ""
		say "  The data, settings and secrets are kept in $DATA_DIR. Run the installer again to"
		say "  start Orglane with them, or '$INSTALLER uninstall --purge' to delete them."
		return 0
	fi
	case $DATA_DIR in
	/ | /var | /var/lib | /home | /Users | "$HOME") die "refusing to delete $DATA_DIR" ;;
	esac
	if ! rm -rf "$DATA_DIR" 2>/dev/null; then
		# Files written by the containers belong to their users (PostgreSQL's,
		# for example); delete them from a container.
		pg_image=$(sed -n 's/^ *image: *\(postgres:[^ ]*\).*/\1/p' "$DATA_DIR/compose.yaml" | head -n 1)
		docker run --rm -v "$DATA_DIR:/data" --entrypoint sh "${pg_image:-alpine}" -c 'rm -rf /data/* /data/.[!.]*' >/dev/null 2>&1 || true
		rm -rf "$DATA_DIR" || die "could not delete $DATA_DIR"
	fi
	ok "deleted $DATA_DIR"
}

# --- Main -----------------------------------------------------------------------------------

cleanup() {
	stop_probe
	if [ -n "${WORK:-}" ]; then rm -rf "$WORK"; fi
}

main() {
	COMMAND=install
	OPT_MODE='' OPT_DOMAIN='' OPT_WORKSPACE_HOST='' OPT_ACME_EMAIL='' OPT_ACME_CA=''
	OPT_DATA_DIR='' OPT_VERSION='' OPT_SOURCE='' OPT_IMAGE='' OPT_HTTP_PORT='' OPT_HTTPS_PORT=''
	OPT_PUBLIC_IP='' OPT_SKIP_CHECKS=0 OPT_INSTALL_DOCKER=0 OPT_PURGE=0 OPT_YES=0
	OLD_HTTP_PORT='' OLD_HTTPS_PORT='' OLD_ACME_CA='' OLD_HTTP_PUBLISH='' OLD_HTTPS_PUBLISH='' PROBE='' WORK=''
	# How to run the installer again, for the summary.
	case $0 in
	*install.sh) INSTALLER="sh $0" ;;
	*) INSTALLER="curl -fsSL https://orglane.app/install.sh | sh -s --" ;;
	esac

	case ${1:-} in
	install | status | uninstall)
		COMMAND=$1
		shift
		;;
	esac
	while [ $# -gt 0 ]; do
		opt=$1
		val=''
		case $opt in
		--*=*)
			val=${opt#*=}
			opt=${opt%%=*}
			;;
		esac
		case $opt in
		--mode | --domain | --workspace-host | --acme-email | --acme-ca | --data-dir | --version | --source | --image | \
			--http-port | --https-port | --public-ip)
			if [ -z "$val" ]; then
				[ $# -ge 2 ] || die "$opt needs a value (see --help)"
				val=$2
				shift
			fi
			;;
		esac
		case $opt in
		--mode)
			case $val in demo | local | domain) OPT_MODE=$val ;; *) die "unknown --mode '$val': use demo, local or domain" ;; esac
			;;
		--domain) OPT_DOMAIN=$val ;;
		--workspace-host) OPT_WORKSPACE_HOST=$val ;;
		--acme-email) OPT_ACME_EMAIL=$val ;;
		--acme-ca) OPT_ACME_CA=$val ;;
		--acme-staging) OPT_ACME_CA=staging ;;
		--data-dir) OPT_DATA_DIR=$val ;;
		--version) OPT_VERSION=$val ;;
		--source)
			[ -d "$val" ] || die "--source $val is not a directory"
			OPT_SOURCE=$(abs_path "$val")
			;;
		--image) OPT_IMAGE=$val ;;
		--http-port) OPT_HTTP_PORT=$val ;;
		--https-port) OPT_HTTPS_PORT=$val ;;
		--public-ip)
			valid_ipv4 "$val" || die "--public-ip needs an IPv4 address, got '$val'"
			OPT_PUBLIC_IP=$val
			;;
		--skip-checks) OPT_SKIP_CHECKS=1 ;;
		--install-docker) OPT_INSTALL_DOCKER=1 ;;
		--purge) OPT_PURGE=1 ;;
		-y | --yes) OPT_YES=1 ;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown option '$opt' (see --help)" ;;
		esac
		shift
	done

	WORK=$(mktemp -d "${TMPDIR:-/tmp}/orglane-install.XXXXXX")
	trap cleanup EXIT
	trap 'exit 130' INT TERM

	case $COMMAND in
	install) cmd_install ;;
	status) cmd_status ;;
	uninstall) cmd_uninstall ;;
	esac
}

main "$@"
