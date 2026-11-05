#!/bin/sh
# Configure hook (runs in the forgejo container after every start with a new
# render): makes the service admin exist with the password Orglane holds.
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
200)
	exit 0
	;;
401 | 403)
	;;
*)
	echo "configure: unexpected status $code from the Forgejo API" >&2
	exit 1
	;;
esac

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
