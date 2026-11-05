#!/bin/sh
# Configure hook (runs in the forgejo container after every start with a new
# render): makes the service admin exist with the password Orglane holds,
# and the "orglane" sign-in source (OpenID Connect with the instance's
# client in Orglane) exist with the current client and issuer.
#
# The password reaches this script only through the environment. curl reads
# it from standard input (printf is a shell builtin, so it never appears in a
# process's arguments); only creating the admin, or resetting a password that
# no longer works, passes it to the forgejo CLI as an argument, because the
# CLI has no other way to take it.
set -eu

user="$ORGLANE_ADMIN_USERNAME"
api="http://127.0.0.1:3000/api/v1"

# Prints the HTTP status of GET /user with the admin's credentials.
status() {
	printf 'user = "%s:%s"\n' "$user" "$ORGLANE_ADMIN_PASSWORD" |
		curl -sS -K - -o /dev/null -w '%{http_code}' "$api/user"
}

code="$(status)"
case "$code" in
200) ;;
401 | 403)
	if forgejo admin user list | awk 'NR > 1 { print $2 }' | grep -qx "$user"; then
		forgejo admin user change-password --username "$user" \
			--password "$ORGLANE_ADMIN_PASSWORD" --must-change-password=false >/dev/null
	else
		forgejo admin user create --admin --username "$user" --email "$ORGLANE_ADMIN_EMAIL" \
			--password "$ORGLANE_ADMIN_PASSWORD" --must-change-password=false >/dev/null
	fi
	code="$(status)"
	if [ "$code" != 200 ]; then
		echo "configure: the service admin cannot sign in (status $code)" >&2
		exit 1
	fi
	;;
*)
	echo "configure: unexpected status $code from the Forgejo API" >&2
	exit 1
	;;
esac

# The sign-in source. Forgejo fetches the discovery document when the
# source is saved, so Orglane must be reachable (it is: it installs the
# service). The client secret is an argument of the CLI, which has no other
# way to take it.
source=orglane
source_id() {
	forgejo admin auth list | awk -F '\t' -v n="$source" 'NR > 1 && $2 == n { print $1 }'
}
set -- --name "$source" --provider openidConnect --key "$ORGLANE_OIDC_CLIENT_ID" \
	--secret "$ORGLANE_OIDC_CLIENT_SECRET" --auto-discover-url "$ORGLANE_OIDC_DISCOVERY_URL" \
	--scopes openid --scopes profile --scopes email --skip-local-2fa
id="$(source_id)"
if [ -z "$id" ]; then
	forgejo admin auth add-oauth "$@" >/dev/null
	id="$(source_id)"
else
	forgejo admin auth update-oauth --id "$id" "$@" >/dev/null
fi
case "$id" in
'' | *[!0-9]*)
	echo "configure: the sign-in source was not created" >&2
	exit 1
	;;
esac

# Forgejo has no API that lists sign-in sources, but Orglane needs this
# one's ID to create accounts bound to it. It is published as a static
# asset (an ID is no secret).
assets=/var/lib/gitea/custom/public/assets/orglane
mkdir -p "$assets"
printf '{"name":"%s","id":%s}\n' "$source" "$id" >"$assets/.auth-source.json.tmp"
mv "$assets/.auth-source.json.tmp" "$assets/auth-source.json"
