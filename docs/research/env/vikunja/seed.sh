#!/bin/sh
# Seeds the local Vikunja: one user, one project, TASK_COUNT tasks (default 500). Prints the
# user's JWT on stdout (for the load test). Local research data only.
set -eu
base=${BASE:-http://127.0.0.1:18101/api/v1}
count=${TASK_COUNT:-500}
case $count in
	''|*[!0-9]*) echo "TASK_COUNT must be an integer from 1 to 500" >&2; exit 2 ;;
esac
[ "$count" -ge 1 ] && [ "$count" -le 500 ] || {
	echo "TASK_COUNT must be an integer from 1 to 500" >&2
	exit 2
}
json='content-type: application/json'
curl -sf -o /dev/null -X POST "$base/register" -H "$json" \
	-d '{"username":"alice","email":"alice@example.com","password":"research-pass-1"}'
token=$(curl -sf -X POST "$base/login" -H "$json" \
	-d '{"username":"alice","password":"research-pass-1"}' | jq -r .token)
auth="authorization: Bearer $token"
project=$(curl -sf -X PUT "$base/projects" -H "$auth" -H "$json" -d '{"title":"Load"}' | jq .id)
i=1
while [ "$i" -le "$count" ]; do
	curl -sf -o /dev/null -X PUT "$base/projects/$project/tasks" -H "$auth" -H "$json" \
		-d "{\"title\":\"Task $i\",\"description\":\"Seeded task number $i for the load test\"}"
	i=$((i + 1))
done
echo "$token"
