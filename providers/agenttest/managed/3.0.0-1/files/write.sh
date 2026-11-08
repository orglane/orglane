#!/bin/sh
# CGI: appends the query string to /data/writes.log and acknowledges it only
# after the write reached the file.
set -eu
printf '%s\n' "${QUERY_STRING:-}" >> /data/writes.log
sync /data/writes.log 2>/dev/null || sync
printf 'Content-Type: text/plain\r\n\r\nok\n'
