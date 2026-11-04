#!/bin/sh
# Serves the "message" setting from the data directory.
set -eu
printf '%s\n' "$MESSAGE" > /data/index.html
exec httpd -f -p 8080 -h /data
