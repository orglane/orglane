#!/bin/sh
# Serves the "message" setting and the package version, and accepts writes
# (GET /cgi-bin/write?<text> appends <text> to /data/writes.log, which
# GET /writes.log returns): the upgrade tests use them as acknowledged
# client writes through the published port.
set -eu
printf '%s\n' "$MESSAGE" > /data/index.html
touch /data/writes.log
# Work of this version on its own: a rollback discards it.
printf '%s\n' "$PACKAGE_VERSION" > /data/written-by
mkdir -p /www/cgi-bin
printf '%s\n' "$PACKAGE_VERSION" > /www/version
ln -sf /data/index.html /www/index.html
ln -sf /data/writes.log /www/writes.log
cp /orglane/files/write.sh /www/cgi-bin/write
chmod 0755 /www/cgi-bin/write
exec httpd -f -p 8080 -h /www
